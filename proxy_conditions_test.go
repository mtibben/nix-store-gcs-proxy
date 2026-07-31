package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBucketProxyHonorsReadPreconditions(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.July, 31, 1, 2, 3, 500, time.UTC)
	tests := map[string]struct {
		headers    map[string]string
		wantStatus int
	}{
		"matching If-Match": {
			headers:    map[string]string{"If-Match": `"gcs-42-3"`},
			wantStatus: http.StatusOK,
		},
		"nonmatching If-Match": {
			headers:    map[string]string{"If-Match": `"old"`},
			wantStatus: http.StatusPreconditionFailed,
		},
		"If-Match wildcard": {
			headers:    map[string]string{"If-Match": "*"},
			wantStatus: http.StatusOK,
		},
		"unmodified since": {
			headers: map[string]string{
				"If-Unmodified-Since": modified.Format(http.TimeFormat),
			},
			wantStatus: http.StatusOK,
		},
		"modified since precondition": {
			headers: map[string]string{
				"If-Unmodified-Since": modified.Add(-time.Second).Format(http.TimeFormat),
			},
			wantStatus: http.StatusPreconditionFailed,
		},
		"matching If-None-Match": {
			headers:    map[string]string{"If-None-Match": `"gcs-42-3"`},
			wantStatus: http.StatusNotModified,
		},
		"weak matching If-None-Match": {
			headers:    map[string]string{"If-None-Match": `W/"gcs-42-3"`},
			wantStatus: http.StatusNotModified,
		},
		"If-None-Match list": {
			headers: map[string]string{
				"If-None-Match": `"old", W/"gcs-42-3", "other"`,
			},
			wantStatus: http.StatusNotModified,
		},
		"If-None-Match wildcard": {
			headers:    map[string]string{"If-None-Match": "*"},
			wantStatus: http.StatusNotModified,
		},
		"nonmatching If-None-Match": {
			headers:    map[string]string{"If-None-Match": `"old"`},
			wantStatus: http.StatusOK,
		},
		"not modified since": {
			headers: map[string]string{
				"If-Modified-Since": modified.Format(http.TimeFormat),
			},
			wantStatus: http.StatusNotModified,
		},
		"modified since": {
			headers: map[string]string{
				"If-Modified-Since": modified.Add(-time.Second).Format(http.TimeFormat),
			},
			wantStatus: http.StatusOK,
		},
		"If-None-Match takes precedence": {
			headers: map[string]string{
				"If-None-Match":     `"old"`,
				"If-Modified-Since": modified.Format(http.TimeFormat),
			},
			wantStatus: http.StatusOK,
		},
		"If-Match takes precedence": {
			headers: map[string]string{
				"If-Match":            `"gcs-42-3"`,
				"If-Unmodified-Since": modified.Add(-time.Second).Format(http.TimeFormat),
			},
			wantStatus: http.StatusOK,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := conditionalObjectStore(modified)
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/example.nar",
				nil,
			)
			for headerName, value := range test.headers {
				request.Header.Set(headerName, value)
			}
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantStatus == http.StatusOK && response.Body.String() != "cache data" {
				t.Errorf("body = %q, want cache data", response.Body.String())
			}
			if test.wantStatus != http.StatusOK && response.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", response.Body.String())
			}
			if test.wantStatus == http.StatusNotModified {
				assertNotModifiedHeaders(t, response.Header())
			}
		})
	}
}

func TestBucketProxyHeadHonorsIfNoneMatch(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodHead,
		"/example.nar",
		nil,
	)
	request.Header.Set("If-None-Match", `"gcs-42-3"`)
	response := httptest.NewRecorder()

	BucketProxy{store: conditionalObjectStore(modified)}.ServeHTTP(response, request)

	if response.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotModified)
	}
	assertNotModifiedHeaders(t, response.Header())
}

func TestBucketProxyHonorsIfRange(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	tests := map[string]struct {
		ifRange    string
		wantStatus int
		wantBody   string
	}{
		"matching ETag": {
			ifRange:    `"gcs-42-3"`,
			wantStatus: http.StatusPartialContent,
			wantBody:   "2345",
		},
		"weak ETag": {
			ifRange:    `W/"gcs-42-3"`,
			wantStatus: http.StatusOK,
			wantBody:   "cache data",
		},
		"stale ETag": {
			ifRange:    `"old"`,
			wantStatus: http.StatusOK,
			wantBody:   "cache data",
		},
		"matching date": {
			ifRange:    modified.Format(http.TimeFormat),
			wantStatus: http.StatusPartialContent,
			wantBody:   "2345",
		},
		"stale date": {
			ifRange:    modified.Add(-time.Second).Format(http.TimeFormat),
			wantStatus: http.StatusOK,
			wantBody:   "cache data",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := conditionalObjectStore(modified)
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/example.nar",
				nil,
			)
			request.Header.Set("Range", "bytes=2-5")
			request.Header.Set("If-Range", test.ifRange)
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if response.Body.String() != test.wantBody {
				t.Errorf("body = %q, want %q", response.Body.String(), test.wantBody)
			}
		})
	}
}

func conditionalObjectStore(modified time.Time) *fakeObjectStore {
	metadata := objectMetadata{
		size:           10,
		contentType:    "application/octet-stream",
		lastModified:   modified,
		generation:     42,
		metageneration: 3,
	}

	return &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return metadata, nil
		},
		newReaderFunc: func(context.Context, string) (objectRead, error) {
			return objectRead{
				body:          io.NopCloser(strings.NewReader("cache data")),
				metadata:      metadata,
				contentLength: 10,
			}, nil
		},
		newRangeReaderFunc: func(
			context.Context,
			string,
			int64,
			int64,
		) (objectRead, error) {
			return objectRead{
				body:          io.NopCloser(strings.NewReader("2345")),
				metadata:      metadata,
				contentLength: 4,
				startOffset:   2,
			}, nil
		},
	}
}

func assertNotModifiedHeaders(t *testing.T, header http.Header) {
	t.Helper()

	if header.Get("ETag") != `"gcs-42-3"` {
		t.Errorf("ETag = %q, want %q", header.Get("ETag"), `"gcs-42-3"`)
	}
	for _, name := range []string{
		"Content-Type",
		"Content-Length",
		"Content-Encoding",
		"Last-Modified",
	} {
		if value := header.Get(name); value != "" {
			t.Errorf("%s = %q, want empty", name, value)
		}
	}
}
