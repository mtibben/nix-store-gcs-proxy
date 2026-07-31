package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

var errBucketNameRequired = errors.New("please specify a bucket name")

type BucketProxy struct {
	store objectStore
}

func (s BucketProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	objectPath := req.URL.Path[1:]

	ctx := req.Context()
	switch req.Method {
	case http.MethodHead:
		metadata, err := s.store.attributes(ctx, objectPath)
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				http.Error(w, "File not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
		setObjectResponseHeaders(w.Header(), metadata, metadata.size)
		w.Header().Set("Accept-Ranges", "bytes")
		if status := readPreconditionStatus(req, metadata); status != 0 {
			writeReadPreconditionResponse(w, status)
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
				if errors.Is(err, errInvalidByteRange) {
					writeRangeNotSatisfiable(w, -1)
				} else {
					s.writeRangeError(w, ctx, objectPath)
				}
				return
			}
			if errors.Is(err, storage.ErrObjectNotExist) {
				http.Error(w, "File not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
		defer func() {
			if err := object.body.Close(); err != nil {
				log.Println(err)
			}
		}()

		setObjectResponseHeaders(w.Header(), object.metadata, object.contentLength)
		if status := readPreconditionStatus(req, object.metadata); status != 0 {
			writeReadPreconditionResponse(w, status)
			return
		}
		if !object.metadata.decompressed {
			w.Header().Set("Accept-Ranges", "bytes")
			if partial {
				setPartialContentHeaders(w.Header(), object)
				w.WriteHeader(http.StatusPartialContent)
			}
		}
		if _, err := io.Copy(w, object.body); err != nil {
			log.Println(err)
		}
	case http.MethodPut:
		// Write the object to GCS
		wc := s.store.newWriter(ctx, objectPath, writeOptionsFromRequest(req))

		if _, err := io.Copy(wc, req.Body); err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := wc.Close(); err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		if _, err := fmt.Fprint(w, "OK"); err != nil {
			log.Println(err)
		}
	default:
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

	if ifRange != "" {
		metadata, err := s.store.attributes(ctx, objectPath)
		if err != nil {
			return objectRead{}, false, err
		}
		if !ifRangeMatches(ifRange, metadata) {
			object, err := s.store.newReader(ctx, objectPath)
			return object, false, err
		}
	}

	byteRange, err := parseObjectByteRange(rangeHeader)
	if err != nil {
		return objectRead{}, false, err
	}

	object, err := s.store.newRangeReader(
		ctx,
		objectPath,
		byteRange.offset,
		byteRange.length,
	)
	return object, true, err
}

func (s BucketProxy) writeRangeError(
	w http.ResponseWriter,
	ctx context.Context,
	objectPath string,
) {
	metadata, err := s.store.attributes(ctx, objectPath)
	if err != nil {
		log.Printf("Read object metadata after range failure: %v", err)
		writeRangeNotSatisfiable(w, -1)
		return
	}

	writeRangeNotSatisfiable(w, metadata.size)
}

func writeOptionsFromRequest(req *http.Request) objectWriteOptions {
	return objectWriteOptions{
		contentType:        req.Header.Get("Content-Type"),
		contentLanguage:    req.Header.Get("Content-Language"),
		contentEncoding:    req.Header.Get("Content-Encoding"),
		contentDisposition: req.Header.Get("Content-Disposition"),
		cacheControl:       req.Header.Get("Cache-Control"),
	}
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
			newCacheReadinessCheck(bucket),
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

func main() {
	app := &cli.Command{
		Name:    "nix-store-gcs-proxy",
		Usage:   "A HTTP nix store that proxies requests to Google Storage",
		Version: "0.0.1",
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

	err := app.Run(context.Background(), os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
