package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBucketProxyGetPreservesObjectMetadata(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	store := &fakeObjectStore{
		newReaderFunc: func(context.Context, string) (objectRead, error) {
			return objectRead{
				body: io.NopCloser(strings.NewReader("cache data")),
				metadata: objectMetadata{
					contentType:        "application/octet-stream",
					contentLanguage:    "en",
					contentEncoding:    "zstd",
					contentDisposition: "attachment",
					cacheControl:       "public, max-age=3600",
					lastModified:       modified,
					generation:         42,
					metageneration:     3,
				},
				contentLength: 10,
			}, nil
		},
	}

	response := serveRequest(
		BucketProxy{store: store},
		http.MethodGet,
		"/example.nar.zst",
	)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); body != "cache data" {
		t.Errorf("body = %q, want cache data", body)
	}
	assertObjectHeaders(t, response.Header(), modified)
}

func TestBucketProxyHeadPreservesObjectMetadata(t *testing.T) {
	t.Parallel()

	modified := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	store := &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{
				size:               10,
				contentType:        "application/octet-stream",
				contentLanguage:    "en",
				contentEncoding:    "zstd",
				contentDisposition: "attachment",
				cacheControl:       "public, max-age=3600",
				lastModified:       modified,
				generation:         42,
				metageneration:     3,
			}, nil
		},
	}

	response := serveRequest(
		BucketProxy{store: store},
		http.MethodHead,
		"/example.nar.zst",
	)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", response.Body.String())
	}
	assertObjectHeaders(t, response.Header(), modified)
}

func TestObjectHeadersOmitEncodingAfterGCSDecompression(t *testing.T) {
	t.Parallel()

	header := make(http.Header)
	setObjectResponseHeaders(
		header,
		objectMetadata{
			contentEncoding: "gzip",
			decompressed:    true,
		},
		-1,
	)

	if encoding := header.Get("Content-Encoding"); encoding != "" {
		t.Errorf("Content-Encoding = %q, want empty", encoding)
	}
	if length := header.Get("Content-Length"); length != "" {
		t.Errorf("Content-Length = %q, want empty", length)
	}
}

func TestBucketProxyServesByteRanges(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rangeHeader string
		wantOffset  int64
		wantLength  int64
		startOffset int64
		body        string
	}{
		"closed": {
			rangeHeader: "bytes=2-5",
			wantOffset:  2,
			wantLength:  4,
			startOffset: 2,
			body:        "2345",
		},
		"case insensitive unit": {
			rangeHeader: "Bytes=2-5",
			wantOffset:  2,
			wantLength:  4,
			startOffset: 2,
			body:        "2345",
		},
		"open ended": {
			rangeHeader: "bytes=6-",
			wantOffset:  6,
			wantLength:  -1,
			startOffset: 6,
			body:        "6789",
		},
		"suffix": {
			rangeHeader: "bytes=-3",
			wantOffset:  -3,
			wantLength:  -1,
			startOffset: 7,
			body:        "789",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := &fakeObjectStore{
				newRangeReaderFunc: func(
					_ context.Context,
					_ string,
					offset, length int64,
				) (objectRead, error) {
					if offset != test.wantOffset {
						t.Errorf("offset = %d, want %d", offset, test.wantOffset)
					}
					if length != test.wantLength {
						t.Errorf("length = %d, want %d", length, test.wantLength)
					}

					return objectRead{
						body: io.NopCloser(strings.NewReader(test.body)),
						metadata: objectMetadata{
							size:           10,
							contentType:    "application/octet-stream",
							generation:     42,
							metageneration: 3,
						},
						contentLength: int64(len(test.body)),
						startOffset:   test.startOffset,
					}, nil
				},
			}
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/example.nar",
				nil,
			)
			request.Header.Set("Range", test.rangeHeader)
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != http.StatusPartialContent {
				t.Errorf(
					"status = %d, want %d",
					response.Code,
					http.StatusPartialContent,
				)
			}
			if body := response.Body.String(); body != test.body {
				t.Errorf("body = %q, want %q", body, test.body)
			}
			if contentLength := response.Header().Get("Content-Length"); contentLength !=
				strconv.Itoa(len(test.body)) {
				t.Errorf(
					"Content-Length = %q, want %d",
					contentLength,
					len(test.body),
				)
			}
			wantContentRange := fmt.Sprintf(
				"bytes %d-%d/10",
				test.startOffset,
				test.startOffset+int64(len(test.body))-1,
			)
			if contentRange := response.Header().Get("Content-Range"); contentRange !=
				wantContentRange {
				t.Errorf("Content-Range = %q, want %q", contentRange, wantContentRange)
			}
			if acceptRanges := response.Header().Get("Accept-Ranges"); acceptRanges != "bytes" {
				t.Errorf("Accept-Ranges = %q, want bytes", acceptRanges)
			}
		})
	}
}

func TestBucketProxyRejectsInvalidByteRanges(t *testing.T) {
	t.Parallel()

	tests := []string{
		"bytes=1-0",
		"bytes=-0",
		"bytes=x-1",
	}

	for _, rangeHeader := range tests {
		t.Run(rangeHeader, func(t *testing.T) {
			t.Parallel()

			store := &fakeObjectStore{
				attributesFunc: func(context.Context, string) (objectMetadata, error) {
					return objectMetadata{size: 10}, nil
				},
			}
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/example.nar",
				nil,
			)
			request.Header.Set("Range", rangeHeader)
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			assertRangeNotSatisfiable(t, response, "bytes */10")
		})
	}
}

func TestBucketProxyIgnoresUnsupportedRanges(t *testing.T) {
	t.Parallel()

	for _, rangeHeader := range []string{
		"items=0-1",
		"bytes=0-1,3-4",
	} {
		t.Run(rangeHeader, func(t *testing.T) {
			t.Parallel()

			fullReads := 0
			store := &fakeObjectStore{
				newReaderFunc: func(context.Context, string) (objectRead, error) {
					fullReads++
					return objectRead{
						body:          io.NopCloser(strings.NewReader("cache data")),
						metadata:      objectMetadata{size: 10},
						contentLength: 10,
					}, nil
				},
			}
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/example.nar",
				nil,
			)
			request.Header.Set("Range", rangeHeader)
			request.Header.Set("If-Range", `"current"`)
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if response.Body.String() != "cache data" {
				t.Errorf("body = %q, want cache data", response.Body.String())
			}
			if fullReads != 1 {
				t.Errorf("full reads = %d, want 1", fullReads)
			}
			if contentRange := response.Header().Get("Content-Range"); contentRange != "" {
				t.Errorf("Content-Range = %q, want empty", contentRange)
			}
		})
	}
}

func TestBucketProxyReportsUnsatisfiableGCSRange(t *testing.T) {
	t.Parallel()

	store := &fakeObjectStore{
		newRangeReaderFunc: func(
			context.Context,
			string,
			int64,
			int64,
		) (objectRead, error) {
			return objectRead{}, errRangeNotSatisfiable
		},
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{size: 10}, nil
		},
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/example.nar",
		nil,
	)
	request.Header.Set("Range", "bytes=20-")
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	assertRangeNotSatisfiable(t, response, "bytes */10")
}

func TestBucketProxyIgnoresRangeAfterGCSDecompression(t *testing.T) {
	t.Parallel()

	store := &fakeObjectStore{
		newRangeReaderFunc: func(
			context.Context,
			string,
			int64,
			int64,
		) (objectRead, error) {
			return objectRead{
				body: io.NopCloser(strings.NewReader("cache data")),
				metadata: objectMetadata{
					size:         10,
					decompressed: true,
				},
				contentLength: 10,
			}, nil
		},
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/example.nar",
		nil,
	)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentRange := response.Header().Get("Content-Range"); contentRange != "" {
		t.Errorf("Content-Range = %q, want empty", contentRange)
	}
	if acceptRanges := response.Header().Get("Accept-Ranges"); acceptRanges != "" {
		t.Errorf("Accept-Ranges = %q, want empty", acceptRanges)
	}
}

func TestBucketProxyAdvertisesSupportedMethods(t *testing.T) {
	t.Parallel()

	response := serveRequest(
		BucketProxy{store: &fakeObjectStore{}},
		http.MethodPost,
		"/example.nar",
	)

	if response.Code != http.StatusMethodNotAllowed {
		t.Errorf(
			"status = %d, want %d",
			response.Code,
			http.StatusMethodNotAllowed,
		)
	}
	if allow := response.Header().Get("Allow"); allow != "GET, HEAD, PUT" {
		t.Errorf("Allow = %q, want GET, HEAD, PUT", allow)
	}
}

func assertRangeNotSatisfiable(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantContentRange string,
) {
	t.Helper()

	if response.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf(
			"status = %d, want %d",
			response.Code,
			http.StatusRequestedRangeNotSatisfiable,
		)
	}
	if contentRange := response.Header().Get("Content-Range"); contentRange != wantContentRange {
		t.Errorf("Content-Range = %q, want %q", contentRange, wantContentRange)
	}
	if acceptRanges := response.Header().Get("Accept-Ranges"); acceptRanges != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", acceptRanges)
	}
}

func assertObjectHeaders(t *testing.T, header http.Header, modified time.Time) {
	t.Helper()

	expected := map[string]string{
		"Content-Length":      "10",
		"Content-Type":        "application/octet-stream",
		"Content-Language":    "en",
		"Content-Encoding":    "zstd",
		"Content-Disposition": "attachment",
		"Cache-Control":       "public, max-age=3600",
		"Last-Modified":       modified.Format(http.TimeFormat),
		"ETag":                `"gcs-42-3"`,
		"Accept-Ranges":       "bytes",
	}

	for name, want := range expected {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

type fakeObjectStore struct {
	attributesFunc     func(context.Context, string) (objectMetadata, error)
	newReaderFunc      func(context.Context, string) (objectRead, error)
	newRangeReaderFunc func(context.Context, string, int64, int64) (objectRead, error)
	newWriterFunc      func(context.Context, string, objectWriteOptions) objectWriter
}

func (s *fakeObjectStore) attributes(
	ctx context.Context,
	objectName string,
) (objectMetadata, error) {
	if s.attributesFunc == nil {
		panic("unexpected attributes call")
	}

	return s.attributesFunc(ctx, objectName)
}

func (s *fakeObjectStore) newReader(
	ctx context.Context,
	objectName string,
) (objectRead, error) {
	if s.newReaderFunc == nil {
		panic("unexpected newReader call")
	}

	return s.newReaderFunc(ctx, objectName)
}

func (s *fakeObjectStore) newRangeReader(
	ctx context.Context,
	objectName string,
	offset, length int64,
) (objectRead, error) {
	if s.newRangeReaderFunc == nil {
		panic("unexpected newRangeReader call")
	}

	return s.newRangeReaderFunc(ctx, objectName, offset, length)
}

func (s *fakeObjectStore) newWriter(
	ctx context.Context,
	objectName string,
	options objectWriteOptions,
) objectWriter {
	if s.newWriterFunc == nil {
		panic("unexpected newWriter call")
	}

	return s.newWriterFunc(ctx, objectName, options)
}

var _ objectStore = (*fakeObjectStore)(nil)

func TestRangeErrorsUseSentinel(t *testing.T) {
	t.Parallel()

	_, err := parseObjectByteRange("bytes=nope")
	if !errors.Is(err, errRangeNotSatisfiable) {
		t.Errorf("error = %v, want errRangeNotSatisfiable", err)
	}
}
