package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
					context.Context,
					string,
					objectWriteOptions,
				) objectWriter {
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
		&failingReader{},
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

type failingReader struct {
	read bool
}

func (r *failingReader) Read(data []byte) (int, error) {
	if r.read {
		return 0, errUploadRead
	}
	r.read = true

	return copy(data, "partial"), errUploadRead
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
