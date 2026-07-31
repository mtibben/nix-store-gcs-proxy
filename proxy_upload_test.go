package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
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

func TestBucketProxySizesUploadBufferFromContentLength(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	writer := &bufferWriteCloser{}
	store := &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{}, nil
		},
		newWriterFunc: func(
			_ context.Context,
			objectName string,
			options objectWriteOptions,
		) objectWriter {
			if objectName != "example.nar" {
				t.Errorf("object name = %q, want example.nar", objectName)
			}
			if options.chunkSize != gcsUploadChunkAlignment {
				t.Errorf(
					"chunk size = %d, want %d",
					options.chunkSize,
					gcsUploadChunkAlignment,
				)
			}

			return writer
		},
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
	if writer.String() != content {
		t.Errorf("uploaded body = %q, want %q", writer.String(), content)
	}
}

func TestBucketProxyUsesPutCreationStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		objectExists bool
		wantStatus   int
	}{
		"create": {
			wantStatus: http.StatusCreated,
		},
		"replace": {
			objectExists: true,
			wantStatus:   http.StatusOK,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := &fakeObjectStore{
				attributesFunc: func(context.Context, string) (objectMetadata, error) {
					if !test.objectExists {
						return objectMetadata{}, storage.ErrObjectNotExist
					}

					return objectMetadata{}, nil
				},
				newWriterFunc: func(
					_ context.Context,
					_ string,
					options objectWriteOptions,
				) objectWriter {
					if !options.conditions.doesNotExist {
						t.Error("ordinary upload was not create-only")
					}
					if !options.reuseExisting {
						t.Error("ordinary upload did not allow identical reuse")
					}
					return &trackingObjectWriter{}
				},
			}
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPut,
				"/example.nar",
				strings.NewReader("cache data"),
			)
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if response.Body.String() != "OK" {
				t.Errorf("body = %q, want OK", response.Body.String())
			}
		})
	}
}

func TestBucketProxyAbortsFailedUpload(t *testing.T) {
	t.Parallel()

	writer := &trackingObjectWriter{}
	store := &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{}, nil
		},
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

func TestBucketProxyReusesIdenticalConcurrentUpload(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	store := concurrentUploadStore(t, content)
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
}

func TestBucketProxyReusesIdenticalConcurrentUploadAfterWriteFailure(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", gcsDefaultUploadChunk+2*streamCopyBufferSize)
	writer := &writeFailingExistingObjectWriter{
		bytesUntilFailure: gcsDefaultUploadChunk,
	}
	store := concurrentUploadStore(t, content)
	store.newWriterFunc = func(
		_ context.Context,
		_ string,
		options objectWriteOptions,
	) objectWriter {
		if !options.reuseExisting {
			t.Error("concurrent upload did not allow identical reuse")
		}
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
	store := concurrentUploadStore(t, content)
	openExisting := store.newReaderFunc
	store.newWriterFunc = func(
		_ context.Context,
		_ string,
		options objectWriteOptions,
	) objectWriter {
		if !options.conditions.doesNotExist {
			t.Error("upload was not create-only")
		}
		if !options.reuseExisting {
			t.Error("upload did not allow identical reuse")
		}
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

func TestBucketProxyRejectsDifferentConcurrentUpload(t *testing.T) {
	t.Parallel()

	store := concurrentUploadStore(t, "different cache data")
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
	store := concurrentUploadStore(t, content)
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

func TestBucketProxyHonorsWritePreconditions(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	metadata := objectMetadata{
		lastModified:   modified,
		generation:     42,
		metageneration: 3,
	}
	matchCurrentGeneration := objectWriteConditions{
		generationMatch:     42,
		metagenerationMatch: 3,
	}

	tests := map[string]struct {
		headers        map[string]string
		objectExists   bool
		wantStatus     int
		wantWrite      bool
		wantConditions objectWriteConditions
		wantReuse      bool
	}{
		"matching If-Match": {
			headers: map[string]string{
				"If-Match": `"gcs-42-3"`,
			},
			objectExists:   true,
			wantStatus:     http.StatusOK,
			wantWrite:      true,
			wantConditions: matchCurrentGeneration,
		},
		"stale If-Match": {
			headers: map[string]string{
				"If-Match": `"old"`,
			},
			objectExists: true,
			wantStatus:   http.StatusPreconditionFailed,
		},
		"If-Match wildcard for missing object": {
			headers: map[string]string{
				"If-Match": "*",
			},
			wantStatus: http.StatusPreconditionFailed,
		},
		"matching If-None-Match": {
			headers: map[string]string{
				"If-None-Match": `"gcs-42-3"`,
			},
			objectExists: true,
			wantStatus:   http.StatusPreconditionFailed,
		},
		"nonmatching If-None-Match": {
			headers: map[string]string{
				"If-None-Match": `"old"`,
			},
			objectExists:   true,
			wantStatus:     http.StatusOK,
			wantWrite:      true,
			wantConditions: matchCurrentGeneration,
		},
		"If-None-Match wildcard for existing object": {
			headers: map[string]string{
				"If-None-Match": "*",
			},
			objectExists: true,
			wantStatus:   http.StatusPreconditionFailed,
		},
		"If-None-Match wildcard for missing object": {
			headers: map[string]string{
				"If-None-Match": "*",
			},
			wantStatus: http.StatusCreated,
			wantWrite:  true,
			wantConditions: objectWriteConditions{
				doesNotExist: true,
			},
		},
		"matching If-Unmodified-Since": {
			headers: map[string]string{
				"If-Unmodified-Since": modified.Format(http.TimeFormat),
			},
			objectExists:   true,
			wantStatus:     http.StatusOK,
			wantWrite:      true,
			wantConditions: matchCurrentGeneration,
		},
		"stale If-Unmodified-Since": {
			headers: map[string]string{
				"If-Unmodified-Since": modified.Add(-time.Second).Format(http.TimeFormat),
			},
			objectExists: true,
			wantStatus:   http.StatusPreconditionFailed,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const content = "cache data"
			attributeCalls := 0
			writerCalls := 0
			writer := &trackingObjectWriter{}
			store := &fakeObjectStore{
				attributesFunc: func(context.Context, string) (objectMetadata, error) {
					attributeCalls++
					if !test.objectExists {
						return objectMetadata{}, storage.ErrObjectNotExist
					}

					return metadata, nil
				},
				newWriterFunc: func(
					_ context.Context,
					_ string,
					options objectWriteOptions,
				) objectWriter {
					writerCalls++
					if options.conditions != test.wantConditions {
						t.Errorf(
							"write conditions = %+v, want %+v",
							options.conditions,
							test.wantConditions,
						)
					}
					if options.reuseExisting != test.wantReuse {
						t.Errorf(
							"reuse existing = %t, want %t",
							options.reuseExisting,
							test.wantReuse,
						)
					}
					return writer
				},
			}
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPut,
				"/example.nar",
				strings.NewReader(content),
			)
			for headerName, value := range test.headers {
				request.Header.Set(headerName, value)
			}
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if attributeCalls != 1 {
				t.Errorf("attribute calls = %d, want 1", attributeCalls)
			}
			wantWriterCalls := 0
			if test.wantWrite {
				wantWriterCalls = 1
			}
			if writerCalls != wantWriterCalls {
				t.Errorf("writer calls = %d, want %d", writerCalls, wantWriterCalls)
			}
			if test.wantWrite && writer.String() != content {
				t.Errorf("uploaded body = %q, want %q", writer.String(), content)
			}
			if test.wantStatus == http.StatusPreconditionFailed {
				if response.Body.Len() != 0 {
					t.Errorf("body = %q, want empty", response.Body.String())
				}
				assertPreconditionFailedHeaders(t, response.Header())
			}
		})
	}
}

func TestBucketProxyReportsConcurrentWritePreconditionFailure(t *testing.T) {
	t.Parallel()

	writer := &trackingObjectWriter{
		closeErr: fmt.Errorf("finish conditional upload: %w", errObjectPreconditionFailed),
	}
	store := &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{
				generation:     42,
				metageneration: 3,
			}, nil
		},
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
		strings.NewReader("cache data"),
	)
	request.Header.Set("If-Match", `"gcs-42-3"`)
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusPreconditionFailed {
		t.Errorf(
			"status = %d, want %d",
			response.Code,
			http.StatusPreconditionFailed,
		)
	}
	if response.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", response.Body.String())
	}
	if !writer.closed {
		t.Error("writer was not closed")
	}
	assertPreconditionFailedHeaders(t, response.Header())
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

func concurrentUploadStore(t *testing.T, existingContent string) *fakeObjectStore {
	t.Helper()

	return &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{}, storage.ErrObjectNotExist
		},
		newWriterFunc: func(
			_ context.Context,
			_ string,
			options objectWriteOptions,
		) objectWriter {
			if !options.reuseExisting {
				t.Error("concurrent upload did not allow identical reuse")
			}
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
