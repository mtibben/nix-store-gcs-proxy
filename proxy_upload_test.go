package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadChunkSize(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		contentLength int64
		want          int
	}{
		"unknown": {
			contentLength: -1,
			want:          gcsDefaultUploadChunk,
		},
		"empty": {
			contentLength: 0,
			want:          gcsUploadChunkAlignment,
		},
		"small": {
			contentLength: 1024,
			want:          gcsUploadChunkAlignment,
		},
		"just below alignment": {
			contentLength: gcsUploadChunkAlignment - 1,
			want:          gcsUploadChunkAlignment,
		},
		"exact alignment": {
			contentLength: gcsUploadChunkAlignment,
			want:          2 * gcsUploadChunkAlignment,
		},
		"just below default": {
			contentLength: gcsDefaultUploadChunk - 1,
			want:          gcsDefaultUploadChunk,
		},
		"default": {
			contentLength: gcsDefaultUploadChunk,
			want:          gcsDefaultUploadChunk,
		},
		"large": {
			contentLength: 10 * gcsDefaultUploadChunk,
			want:          gcsDefaultUploadChunk,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := uploadChunkSize(test.contentLength); got != test.want {
				t.Errorf("uploadChunkSize(%d) = %d, want %d", test.contentLength, got, test.want)
			}
		})
	}
}

func TestBucketProxyUsesNixUploadHeadersAndContentLength(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	writer := &bufferWriteCloser{}
	store := &fakeObjectStore{
		newWriterFunc: func(
			_ context.Context,
			objectName string,
			options objectWriteOptions,
		) objectWriter {
			if objectName != "example.narinfo" {
				t.Errorf("object name = %q, want example.narinfo", objectName)
			}
			if options.chunkSize != gcsUploadChunkAlignment {
				t.Errorf(
					"chunk size = %d, want %d",
					options.chunkSize,
					gcsUploadChunkAlignment,
				)
			}
			if options.contentType != "text/x-nix-narinfo" {
				t.Errorf("content type = %q, want text/x-nix-narinfo", options.contentType)
			}
			if options.contentEncoding != "zstd" {
				t.Errorf("content encoding = %q, want zstd", options.contentEncoding)
			}

			return writer
		},
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.narinfo",
		strings.NewReader(content),
	)
	request.Header.Set("Content-Type", "text/x-nix-narinfo")
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	if writer.String() != content {
		t.Errorf("uploaded body = %q, want %q", writer.String(), content)
	}
}

func TestBucketProxyAbortsFailedUpload(t *testing.T) {
	t.Parallel()

	writer := &trackingObjectWriter{}
	store := &fakeObjectStore{
		newWriterFunc: func(
			context.Context,
			string,
			objectWriteOptions,
		) objectWriter {
			return writer
		},
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.nar",
		&singleReadResultReader{
			content: "partial",
			err:     errUploadRead,
		},
	)
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
	if !writer.aborted {
		t.Error("failed upload was not aborted")
	}
	if writer.closed {
		t.Error("failed upload was closed as successful")
	}
}

func TestBucketProxyReusesIdenticalNixUpload(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	store := concurrentUploadStore(content)
	store.newReaderFunc = func(context.Context, string) (objectRead, error) {
		return objectRead{
			body: io.NopCloser(strings.NewReader(content)),
			metadata: objectMetadata{
				contentType:     "text/x-nix-narinfo",
				contentEncoding: "zstd",
			},
		}, nil
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.narinfo",
		strings.NewReader(content),
	)
	request.Header.Set("Content-Type", "text/x-nix-narinfo")
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); body != "OK" {
		t.Errorf("body = %q, want OK", body)
	}
}

func TestBucketProxyKeepsCollisionTargetStableWhileReadingBody(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	comparedObject := ""
	type contextKey struct{}
	key := contextKey{}
	originalContext := context.WithValue(context.Background(), key, "original")
	comparedContext := ""
	store := concurrentUploadStore(content)
	store.newReaderFunc = func(ctx context.Context, objectName string) (objectRead, error) {
		comparedObject = objectName
		contextValue, ok := ctx.Value(key).(string)
		if !ok {
			t.Fatal("comparison context did not contain the expected value")
		}
		comparedContext = contextValue
		return objectRead{
			body: io.NopCloser(strings.NewReader(content)),
		}, nil
	}
	request := httptest.NewRequestWithContext(
		originalContext,
		http.MethodPut,
		"/example.nar",
		nil,
	)
	body := strings.NewReader(content)
	request.Body = io.NopCloser(readerFunc(func(data []byte) (int, error) {
		request.URL.Path = "/different.nar"
		*request = *request.WithContext(
			context.WithValue(context.Background(), key, "different"),
		)
		return body.Read(data)
	}))
	request.ContentLength = int64(len(content))
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if comparedObject != "example.nar" {
		t.Errorf("compared object = %q, want example.nar", comparedObject)
	}
	if comparedContext != "original" {
		t.Errorf("compared context = %q, want original", comparedContext)
	}
}

func TestBucketProxyReusesIdenticalConcurrentUploadAfterWriteFailure(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", gcsDefaultUploadChunk+2*streamCopyBufferSize)
	writer := &writeFailingExistingObjectWriter{
		bytesUntilFailure: gcsDefaultUploadChunk,
	}
	store := concurrentUploadStore(content)
	store.newWriterFunc = func(
		_ context.Context,
		_ string,
		_ objectWriteOptions,
	) objectWriter {
		return writer
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.nar",
		strings.NewReader(content),
	)
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); body != "OK" {
		t.Errorf("body = %q, want OK", body)
	}
	if !writer.closed {
		t.Error("failed writer was not closed to recover its storage error")
	}
	if writer.aborted {
		t.Error("failed writer was aborted before its storage error was recovered")
	}
}

func TestBucketProxyDoesNotReuseUploadAfterSimultaneousReadAndWriteFailure(
	t *testing.T,
) {
	t.Parallel()

	const content = "partial"
	writer := &writeFailingExistingObjectWriter{}
	readerCalls := 0
	store := concurrentUploadStore(content)
	openExisting := store.newReaderFunc
	store.newWriterFunc = func(
		_ context.Context,
		_ string,
		_ objectWriteOptions,
	) objectWriter {
		return writer
	}
	store.newReaderFunc = func(ctx context.Context, name string) (objectRead, error) {
		readerCalls++
		return openExisting(ctx, name)
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.nar",
		&singleReadResultReader{
			content: content,
			err:     errUploadRead,
		},
	)
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
	if !strings.Contains(response.Body.String(), errUploadRead.Error()) {
		t.Errorf("body = %q, want upload read error", response.Body.String())
	}
	if readerCalls != 0 {
		t.Errorf("existing object reads = %d, want 0", readerCalls)
	}
	if !writer.aborted {
		t.Error("writer was not aborted")
	}
	if writer.closed {
		t.Error("writer was closed after the request body failed")
	}
}

func TestReadErrorRecorderAcceptsDataWithEOF(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	reader := &readErrorRecorder{
		reader: &singleReadResultReader{
			content: content,
			err:     io.EOF,
		},
	}
	data := make([]byte, len(content))

	read, err := reader.Read(data)
	if read != len(content) {
		t.Errorf("read = %d, want %d", read, len(content))
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("error = %v, want EOF", err)
	}
	if string(data) != content {
		t.Errorf("content = %q, want %q", data, content)
	}
	if reader.err != nil {
		t.Errorf("recorded error = %v, want nil", reader.err)
	}
}

func TestStreamUploadAbortsAfterShortWriteWithoutError(t *testing.T) {
	t.Parallel()

	writer := &shortWritingObjectWriter{}

	result, err := streamUpload(
		"example.nar",
		writer,
		strings.NewReader("cache data"),
	)

	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("error = %v, want short write", err)
	}
	if result.size != 0 || len(result.digest) != 0 || result.alreadyExists {
		t.Errorf("result = %+v, want empty result", result)
	}
	if !writer.aborted {
		t.Error("short-writing writer was not aborted")
	}
	if writer.closed {
		t.Error("short-writing writer was closed")
	}
}

func TestStreamUploadReportsReadFailureWhileFinishingCollisionHash(t *testing.T) {
	t.Parallel()

	writer := &writeFailingExistingObjectWriter{}
	reader := &readThenErrorReader{
		content: "partial",
		err:     errUploadRead,
	}

	result, err := streamUpload("example.nar", writer, reader)

	if !errors.Is(err, errUploadRead) {
		t.Errorf("error = %v, want upload read error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "finish hashing upload") {
		t.Errorf("error = %q, want finish hashing context", err)
	}
	if result.size != 0 || len(result.digest) != 0 || result.alreadyExists {
		t.Errorf("result = %+v, want empty result", result)
	}
	if !writer.closed {
		t.Error("failed writer was not closed to recover its storage error")
	}
	if writer.aborted {
		t.Error("failed writer was aborted before its storage error was recovered")
	}
}

func TestBucketProxyRejectsDifferentConcurrentUpload(t *testing.T) {
	t.Parallel()

	store := concurrentUploadStore("different cache data")
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.nar",
		strings.NewReader("cache data"),
	)
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func TestBucketProxyRejectsConcurrentUploadWithDifferentMetadata(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	store := concurrentUploadStore(content)
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/example.nar",
		strings.NewReader(content),
	)
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

type bufferWriteCloser struct {
	bytes.Buffer
}

func (*bufferWriteCloser) Close() error {
	return nil
}

func (*bufferWriteCloser) abort() {}

var errUploadRead = errors.New("upload read failed")

type singleReadResultReader struct {
	content string
	err     error
	read    bool
}

func (r *singleReadResultReader) Read(data []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true

	return copy(data, r.content), r.err
}

type readThenErrorReader struct {
	content string
	err     error
	read    bool
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(data []byte) (int, error) {
	return f(data)
}

func (r *readThenErrorReader) Read(data []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true

	return copy(data, r.content), nil
}

type trackingObjectWriter struct {
	bytes.Buffer
	aborted  bool
	closed   bool
	closeErr error
}

func (w *trackingObjectWriter) Close() error {
	w.closed = true
	return w.closeErr
}

func (w *trackingObjectWriter) abort() {
	w.aborted = true
}

type shortWritingObjectWriter struct {
	aborted bool
	closed  bool
}

func (*shortWritingObjectWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return len(data) - 1, nil
}

func (w *shortWritingObjectWriter) Close() error {
	w.closed = true
	return nil
}

func (w *shortWritingObjectWriter) abort() {
	w.aborted = true
}

func concurrentUploadStore(existingContent string) *fakeObjectStore {
	return &fakeObjectStore{
		newWriterFunc: func(
			_ context.Context,
			_ string,
			_ objectWriteOptions,
		) objectWriter {
			return &existingObjectWriter{}
		},
		newReaderFunc: func(context.Context, string) (objectRead, error) {
			return objectRead{
				body: io.NopCloser(strings.NewReader(existingContent)),
			}, nil
		},
	}
}

type existingObjectWriter struct {
	bytes.Buffer
}

func (*existingObjectWriter) Close() error {
	return errObjectAlreadyExists
}

func (*existingObjectWriter) abort() {}

var errUploadWrite = errors.New("upload write failed")

type writeFailingExistingObjectWriter struct {
	bytesUntilFailure int
	closed            bool
	aborted           bool
}

func (w *writeFailingExistingObjectWriter) Write(data []byte) (int, error) {
	if w.bytesUntilFailure == 0 {
		return 0, errUploadWrite
	}

	written := min(len(data), w.bytesUntilFailure)
	w.bytesUntilFailure -= written
	if written < len(data) {
		return written, errUploadWrite
	}
	return written, nil
}

func (w *writeFailingExistingObjectWriter) Close() error {
	w.closed = true
	return errObjectAlreadyExists
}

func (w *writeFailingExistingObjectWriter) abort() {
	w.aborted = true
}
