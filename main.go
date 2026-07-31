package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
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

type uploadResult struct {
	size          int64
	digest        []byte
	alreadyExists bool
}

type uploadExpectation struct {
	size    int64
	digest  []byte
	options objectWriteOptions
}

const (
	gcsUploadChunkAlignment = 256 * 1024
	gcsDefaultUploadChunk   = 16 * 1024 * 1024
)

func (s BucketProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	objectPath := req.URL.Path[1:]

	switch req.Method {
	case http.MethodHead:
		s.serveHead(w, req, objectPath)
	case http.MethodGet:
		s.serveGet(w, req, objectPath)
	case http.MethodPut:
		s.servePut(w, req, objectPath)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		msg := fmt.Sprintf("Method '%s' is not supported", req.Method)
		http.Error(w, msg, http.StatusMethodNotAllowed)
	}
}

func (s BucketProxy) serveHead(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	metadata, err := s.store.attributes(req.Context(), objectPath)
	if err != nil {
		writeObjectReadError(w, err)
		return
	}
	setObjectResponseHeaders(w.Header(), metadata, metadata.size)
	w.Header().Set("Accept-Ranges", "bytes")
	if status := readPreconditionStatus(req, metadata); status != 0 {
		writePreconditionResponse(w, status)
	}
}

func (s BucketProxy) serveGet(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	object, partial, err := s.readObject(req, objectPath)
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
}

func (s BucketProxy) servePut(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	ctx := req.Context()
	plan, status, err := s.writePlanForRequest(req, objectPath)
	if err != nil {
		writeObjectReadError(w, err)
		return
	}
	if status != 0 {
		writePreconditionResponse(w, status)
		return
	}

	writer := s.store.newWriter(ctx, objectPath, plan.options)
	upload, err := streamUpload(objectPath, writer, req.Body)
	if err != nil {
		writeObjectWriteError(w, err)
		return
	}
	if upload.alreadyExists {
		want := uploadExpectation{
			size:    upload.size,
			digest:  upload.digest,
			options: plan.options,
		}
		s.writeExistingUploadResponse(w, ctx, objectPath, want)
		return
	}

	if plan.created {
		w.WriteHeader(http.StatusCreated)
	}
	writeUploadSuccess(w)
}

func streamUpload(
	objectPath string,
	wc objectWriter,
	source io.Reader,
) (uploadResult, error) {
	digest := sizedHash{Hash: sha256.New()}
	bodyReader := &readErrorRecorder{reader: source}
	body := io.TeeReader(bodyReader, &digest)
	writer := &writeErrorRecorder{writer: wc}

	_, copyErr := copyStream(writer, body)
	if copyErr == nil {
		closeErr := wc.Close()
		return completedUploadResult(&digest, closeErr)
	}

	if bodyReader.err != nil {
		wc.abort()
		return uploadResult{}, fmt.Errorf(
			"stream upload %q: %w",
			objectPath,
			bodyReader.err,
		)
	}
	if writer.err == nil {
		wc.abort()
		return uploadResult{}, fmt.Errorf(
			"stream upload %q: %w",
			objectPath,
			copyErr,
		)
	}

	uploadErr := fmt.Errorf("stream upload %q: %w", objectPath, copyErr)
	uploadErr = errors.Join(uploadErr, wc.Close())
	if !errors.Is(uploadErr, errObjectAlreadyExists) {
		return uploadResult{}, uploadErr
	}

	if _, err := copyStream(io.Discard, body); err != nil {
		return uploadResult{}, fmt.Errorf(
			"finish hashing upload %q: %w",
			objectPath,
			err,
		)
	}

	return uploadResult{
		size:          digest.size,
		digest:        digest.Sum(nil),
		alreadyExists: true,
	}, nil
}

func completedUploadResult(digest *sizedHash, closeErr error) (uploadResult, error) {
	result := uploadResult{
		size:   digest.size,
		digest: digest.Sum(nil),
	}
	if closeErr == nil {
		return result, nil
	}
	if errors.Is(closeErr, errObjectAlreadyExists) {
		result.alreadyExists = true
		return result, nil
	}

	return uploadResult{}, closeErr
}

type readErrorRecorder struct {
	reader io.Reader
	err    error
}

func (r *readErrorRecorder) Read(data []byte) (int, error) {
	read, err := r.reader.Read(data)
	if err == nil {
		return read, nil
	}
	if err == io.EOF {
		return read, io.EOF
	}

	wrappedErr := fmt.Errorf("read upload body: %w", err)
	if r.err == nil {
		r.err = wrappedErr
	}
	return read, wrappedErr
}

type sizedHash struct {
	hash.Hash
	size int64
}

func (h *sizedHash) Write(data []byte) (int, error) {
	written, err := h.Hash.Write(data)
	h.size += int64(written)
	if err != nil {
		return written, fmt.Errorf("hash upload: %w", err)
	}
	return written, nil
}

type writeErrorRecorder struct {
	writer io.Writer
	err    error
}

func (w *writeErrorRecorder) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if err != nil {
		err = fmt.Errorf("write upload: %w", err)
	}
	w.err = err
	return written, err
}

func (s BucketProxy) writeExistingUploadResponse(
	w http.ResponseWriter,
	ctx context.Context,
	objectPath string,
	want uploadExpectation,
) {
	identical, err := s.objectMatches(ctx, objectPath, want)
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if identical {
		writeUploadSuccess(w)
		return
	}

	conflictErr := fmt.Errorf("%w: %q", errObjectContentConflict, objectPath)
	log.Println(conflictErr)
	http.Error(w, conflictErr.Error(), http.StatusConflict)
}

func (s BucketProxy) objectMatches(
	ctx context.Context,
	objectPath string,
	want uploadExpectation,
) (bool, error) {
	object, err := s.store.newReader(ctx, objectPath)
	if err != nil {
		return false, fmt.Errorf(
			"read concurrently-created object %q: %w",
			objectPath,
			err,
		)
	}

	digest := sha256.New()
	size, copyErr := copyStream(digest, object.body)
	closeErr := object.body.Close()
	if copyErr != nil {
		return false, fmt.Errorf(
			"hash concurrently-created object %q: %w",
			objectPath,
			copyErr,
		)
	}
	if closeErr != nil {
		return false, fmt.Errorf(
			"close concurrently-created object %q: %w",
			objectPath,
			closeErr,
		)
	}

	return size == want.size &&
		bytes.Equal(digest.Sum(nil), want.digest) &&
		objectMetadataMatchesWriteOptions(object.metadata, want.options), nil
}

func objectMetadataMatchesWriteOptions(
	metadata objectMetadata,
	options objectWriteOptions,
) bool {
	// GCS derives Content-Type and may apply a default Cache-Control value when
	// those request headers are absent, so blank values do not constrain them.
	contentTypeMatches := options.contentType == "" ||
		metadata.contentType == options.contentType
	cacheControlMatches := options.cacheControl == "" ||
		metadata.cacheControl == options.cacheControl

	return contentTypeMatches &&
		metadata.contentLanguage == options.contentLanguage &&
		metadata.contentEncoding == options.contentEncoding &&
		metadata.contentDisposition == options.contentDisposition &&
		cacheControlMatches
}

func writeUploadSuccess(w http.ResponseWriter) {
	if _, err := fmt.Fprint(w, "OK"); err != nil {
		log.Println(err)
	}
}

func (s BucketProxy) readObject(
	req *http.Request,
	objectPath string,
) (objectRead, bool, error) {
	ctx := req.Context()
	rangeHeader := req.Header.Get("Range")
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
		return s.readAfterRangeFailure(req, objectPath, err)
	}

	object, err := s.store.newRangeReader(ctx, objectPath, byteRange)
	if err != nil {
		return s.readAfterRangeFailure(req, objectPath, err)
	}
	ifRange := req.Header.Get("If-Range")
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
	req *http.Request,
	objectPath string,
	rangeErr error,
) (objectRead, bool, error) {
	ifRange := req.Header.Get("If-Range")
	if ifRange == "" {
		return objectRead{}, false, rangeErr
	}

	metadata, err := s.store.attributes(req.Context(), objectPath)
	if err != nil {
		return objectRead{}, false, err
	}
	if ifRangeMatches(ifRange, metadata) {
		return objectRead{}, false, rangeErr
	}

	object, err := s.store.newReader(req.Context(), objectPath)
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
	req *http.Request,
	objectPath string,
) (objectWritePlan, int, error) {
	options := writeOptionsFromRequest(req)
	metadata, err := s.store.attributes(req.Context(), objectPath)
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
	status, applied := writePreconditionStatus(req.Header, metadata, exists)
	if status != 0 {
		return plan, status, nil
	}
	if !applied {
		plan.options.conditions.doesNotExist = true
		plan.options.reuseExisting = true
		return plan, 0, nil
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
func run(
	parentCtx context.Context,
	addr, bucketName, buildVersion string,
) (runErr error) {
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
	log.Print(startupLogMessage(buildVersion, addr, bucketName))

	return serveUntilShutdown(ctx, server)
}

func serveUntilShutdown(ctx context.Context, server *http.Server) error {
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

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

func startupLogMessage(buildVersion, addr, bucketName string) string {
	return fmt.Sprintf(
		"Starting nix-store-gcs-proxy version %s on address %s for bucket %s",
		buildVersion,
		addr,
		bucketName,
	)
}

// Urfave CLI action.
func action(ctx context.Context, c *cli.Command, buildVersion string) error {
	addr := c.String("addr")
	bucketName := c.String("bucket-name")
	if bucketName == "" {
		return errBucketNameRequired
	}
	return run(ctx, addr, bucketName, buildVersion)
}

func newCommand(buildVersion string) *cli.Command {
	return &cli.Command{
		Name:    "nix-store-gcs-proxy",
		Usage:   "A HTTP nix store that proxies requests to Google Storage",
		Version: buildVersion,
		Action: func(ctx context.Context, c *cli.Command) error {
			return action(ctx, c, buildVersion)
		},
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
