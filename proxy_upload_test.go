package main

import (
	"bytes"
	"context"
	"errors"
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

func TestBucketProxySizesUploadBufferFromContentLength(t *testing.T) {
	t.Parallel()

	const content = "cache data"
	writer := &bufferWriteCloser{}
	store := &fakeObjectStore{
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
	aborted bool
	closed  bool
}

func (w *trackingObjectWriter) Close() error {
	w.closed = true
	return nil
}

func (w *trackingObjectWriter) abort() {
	w.aborted = true
}
