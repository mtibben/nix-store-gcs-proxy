package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/urfave/cli/v3"
	"google.golang.org/api/option"
)

const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 60 * time.Second
	serverShutdownTimeout   = 10 * time.Second
)

var (
	version               = "dev"
	errBucketNameRequired = errors.New("please specify a bucket name")
)

type BucketProxy struct {
	store objectStore
}

type objectWritePlan struct {
	options objectWriteOptions
	created bool
}

const (
	gcsUploadChunkAlignment = 256 * 1024
	gcsDefaultUploadChunk   = 16 * 1024 * 1024
)

func (s BucketProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	objectPath := req.URL.Path[1:]

	ctx := req.Context()
	switch req.Method {
	case http.MethodHead:
		metadata, err := s.store.attributes(ctx, objectPath)
		if err != nil {
			writeObjectReadError(w, err)
			return
		}
		setObjectResponseHeaders(w.Header(), metadata, metadata.size)
		w.Header().Set("Accept-Ranges", "bytes")
		if status := readPreconditionStatus(req, metadata); status != 0 {
			writePreconditionResponse(w, status)
			return
		}
	case http.MethodGet:
		object, partial, err := s.readObject(
			ctx,
			objectPath,
			req.Header.Get("Range"),
			req.Header.Get("If-Range"),
		)
		if err != nil {
			if errors.Is(err, errRangeNotSatisfiable) {
				s.writeRangeError(w, req, objectPath)
				return
			}
			writeObjectReadError(w, err)
			return
		}
		defer func() {
			if err := object.body.Close(); err != nil {
				log.Println(err)
			}
		}()

		setObjectResponseHeaders(w.Header(), object.metadata, object.contentLength)
		if status := readPreconditionStatus(req, object.metadata); status != 0 {
			writePreconditionResponse(w, status)
			return
		}
		if !object.metadata.decompressed {
			w.Header().Set("Accept-Ranges", "bytes")
			if partial {
				setPartialContentHeaders(w.Header(), object)
				w.WriteHeader(http.StatusPartialContent)
			}
		}
		if _, err := copyStream(w, object.body); err != nil {
			log.Println(err)
		}
	case http.MethodPut:
		plan, status, err := s.writePlanForRequest(ctx, objectPath, req)
		if err != nil {
			writeObjectReadError(w, err)
			return
		}
		if status != 0 {
			writePreconditionResponse(w, status)
			return
		}

		wc := s.store.newWriter(ctx, objectPath, plan.options)

		if _, err := copyStream(wc, req.Body); err != nil {
			wc.abort()
			uploadErr := fmt.Errorf("stream upload %q: %w", objectPath, err)
			writeObjectWriteError(w, uploadErr)
			return
		}
		if err := wc.Close(); err != nil {
			writeObjectWriteError(w, err)
			return
		}

		if plan.created {
			w.WriteHeader(http.StatusCreated)
		}
		if _, err := fmt.Fprint(w, "OK"); err != nil {
			log.Println(err)
		}
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		msg := fmt.Sprintf("Method '%s' is not supported", req.Method)
		http.Error(w, msg, http.StatusMethodNotAllowed)
	}
}

func (s BucketProxy) readObject(
	ctx context.Context,
	objectPath, rangeHeader, ifRange string,
) (objectRead, bool, error) {
	if rangeHeader == "" {
		object, err := s.store.newReader(ctx, objectPath)
		return object, false, err
	}

	byteRange, err := parseObjectByteRange(rangeHeader)
	if err != nil {
		if errors.Is(err, errRangeNotSupported) {
			object, readErr := s.store.newReader(ctx, objectPath)
			return object, false, readErr
		}
		return s.readAfterRangeFailure(ctx, objectPath, ifRange, err)
	}

	object, err := s.store.newRangeReader(
		ctx,
		objectPath,
		byteRange.offset,
		byteRange.length,
	)
	if err != nil {
		return s.readAfterRangeFailure(ctx, objectPath, ifRange, err)
	}
	if ifRange == "" || ifRangeMatches(ifRange, object.metadata) {
		return object, true, nil
	}

	if err := object.body.Close(); err != nil {
		return objectRead{}, false, fmt.Errorf(
			"close stale object %q range: %w",
			objectPath,
			err,
		)
	}

	fullObject, err := s.store.newReader(ctx, objectPath)
	return fullObject, false, err
}

func (s BucketProxy) readAfterRangeFailure(
	ctx context.Context,
	objectPath, ifRange string,
	rangeErr error,
) (objectRead, bool, error) {
	if ifRange == "" {
		return objectRead{}, false, rangeErr
	}

	metadata, err := s.store.attributes(ctx, objectPath)
	if err != nil {
		return objectRead{}, false, err
	}
	if ifRangeMatches(ifRange, metadata) {
		return objectRead{}, false, rangeErr
	}

	object, err := s.store.newReader(ctx, objectPath)
	return object, false, err
}

func (s BucketProxy) writeRangeError(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	metadata, err := s.store.attributes(req.Context(), objectPath)
	if err != nil {
		writeObjectReadError(w, err)
		return
	}

	if status := readPreconditionStatus(req, metadata); status != 0 {
		setObjectResponseHeaders(w.Header(), metadata, metadata.size)
		w.Header().Set("Accept-Ranges", "bytes")
		writePreconditionResponse(w, status)
		return
	}

	writeRangeNotSatisfiable(w, metadata.size)
}

func writeObjectReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrObjectNotExist) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	http.Error(w, err.Error(), http.StatusBadGateway)
}

func writeObjectWriteError(w http.ResponseWriter, err error) {
	if errors.Is(err, errObjectPreconditionFailed) {
		writePreconditionResponse(w, http.StatusPreconditionFailed)
		return
	}

	log.Println(err)
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func (s BucketProxy) writePlanForRequest(
	ctx context.Context,
	objectPath string,
	req *http.Request,
) (objectWritePlan, int, error) {
	options := writeOptionsFromRequest(req)
	metadata, err := s.store.attributes(ctx, objectPath)
	exists := true
	if errors.Is(err, storage.ErrObjectNotExist) {
		exists = false
		metadata = objectMetadata{}
	} else if err != nil {
		return objectWritePlan{}, 0, err
	}

	plan := objectWritePlan{
		options: options,
		created: !exists,
	}
	if !hasWritePreconditions(req.Header) {
		return plan, 0, nil
	}

	status, applied := writePreconditionStatus(req.Header, metadata, exists)
	if status != 0 || !applied {
		return plan, status, nil
	}

	if exists {
		plan.options.conditions.generationMatch = metadata.generation
		plan.options.conditions.metagenerationMatch = metadata.metageneration
	} else {
		plan.options.conditions.doesNotExist = true
	}

	return plan, 0, nil
}

func writeOptionsFromRequest(req *http.Request) objectWriteOptions {
	return objectWriteOptions{
		contentType:        req.Header.Get("Content-Type"),
		contentLanguage:    req.Header.Get("Content-Language"),
		contentEncoding:    req.Header.Get("Content-Encoding"),
		contentDisposition: req.Header.Get("Content-Disposition"),
		cacheControl:       req.Header.Get("Cache-Control"),
		chunkSize:          uploadChunkSize(req.ContentLength),
	}
}

func uploadChunkSize(contentLength int64) int {
	if contentLength < 0 || contentLength >= gcsDefaultUploadChunk {
		return gcsDefaultUploadChunk
	}

	sizeWithFinalByte := contentLength + 1
	alignedSize := (sizeWithFinalByte + gcsUploadChunkAlignment - 1) /
		gcsUploadChunkAlignment *
		gcsUploadChunkAlignment

	return int(alignedSize)
}

// Start the HTTP server
func run(parentCtx context.Context, addr, bucketName string) (runErr error) {
	ctx, stop := signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := storage.NewClient(ctx, option.WithScopes(storage.ScopeReadWrite))
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close storage client: %w", err))
		}
	}()

	bucket := client.Bucket(bucketName)
	store := newGCSObjectStore(bucket)

	server := &http.Server{
		Addr: addr,
		Handler: newHTTPHandler(
			BucketProxy{store: store},
			cacheReadinessCheck(
				newCacheReadinessCheck(bucket),
				healthReadinessCacheTTL,
			),
			healthReadinessTimeout,
		),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	log.Printf("Starting proxy server on address %s for bucket %s", addr, bucketName)

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		serverShutdownTimeout,
	)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		shutdownErr := fmt.Errorf("shut down HTTP server: %w", err)
		if err := server.Close(); err != nil {
			return errors.Join(shutdownErr, fmt.Errorf("close HTTP server: %w", err))
		}
		return shutdownErr
	}

	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}

	return nil
}

// Urfave cli action
func action(ctx context.Context, c *cli.Command) error {
	addr := c.String("addr")
	bucketName := c.String("bucket-name")
	if bucketName == "" {
		return errBucketNameRequired
	}
	return run(ctx, addr, bucketName)
}

func newCommand(buildVersion string) *cli.Command {
	return &cli.Command{
		Name:    "nix-store-gcs-proxy",
		Usage:   "A HTTP nix store that proxies requests to Google Storage",
		Version: buildVersion,
		Action:  action,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "bucket-name",
				Usage: "name of the bucket to proxy the data to",
			},
			&cli.StringFlag{
				Name:  "addr",
				Value: "localhost:3000",
				Usage: "listening address of the HTTP server",
			},
		},
	}
}

func main() {
	app := newCommand(version)
	err := app.Run(context.Background(), os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
