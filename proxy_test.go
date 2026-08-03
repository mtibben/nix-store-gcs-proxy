package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBucketProxyGetPreservesObjectMetadata(t *testing.T) {
	t.Parallel()

	store := &fakeObjectStore{
		newReaderFunc: func(context.Context, string) (objectRead, error) {
			return objectRead{
				body: io.NopCloser(strings.NewReader("cache data")),
				metadata: objectMetadata{
					contentType:     "application/octet-stream",
					contentEncoding: "zstd",
					cacheControl:    "public, max-age=3600",
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
	assertObjectHeaders(t, response.Header())
}

func TestBucketProxyHeadPreservesObjectMetadata(t *testing.T) {
	t.Parallel()

	store := &fakeObjectStore{
		attributesFunc: func(context.Context, string) (objectMetadata, error) {
			return objectMetadata{
				size:            10,
				contentType:     "application/octet-stream",
				contentEncoding: "zstd",
				cacheControl:    "public, max-age=3600",
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
	assertObjectHeaders(t, response.Header())
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

func TestBucketProxyServesNixResumeRange(t *testing.T) {
	t.Parallel()

	for _, rangeHeader := range []string{"bytes=6-", "Bytes=6-"} {
		t.Run(rangeHeader, func(t *testing.T) {
			t.Parallel()

			store := &fakeObjectStore{
				newRangeReaderFunc: func(
					_ context.Context,
					_ string,
					offset int64,
				) (objectRead, error) {
					if offset != 6 {
						t.Errorf("range offset = %d, want 6", offset)
					}
					return objectRead{
						body:          io.NopCloser(strings.NewReader("6789")),
						metadata:      objectMetadata{size: 10, contentType: "application/octet-stream"},
						contentLength: 4,
						startOffset:   6,
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
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != http.StatusPartialContent {
				t.Errorf("status = %d, want %d", response.Code, http.StatusPartialContent)
			}
			if body := response.Body.String(); body != "6789" {
				t.Errorf("body = %q, want 6789", body)
			}
			if length := response.Header().Get("Content-Length"); length != "4" {
				t.Errorf("Content-Length = %q, want 4", length)
			}
			if contentRange := response.Header().Get("Content-Range"); contentRange != "bytes 6-9/10" {
				t.Errorf("Content-Range = %q, want bytes 6-9/10", contentRange)
			}
			if acceptRanges := response.Header().Get("Accept-Ranges"); acceptRanges != "bytes" {
				t.Errorf("Accept-Ranges = %q, want bytes", acceptRanges)
			}
		})
	}
}

func TestBucketProxyIgnoresNonNixRanges(t *testing.T) {
	t.Parallel()

	for _, rangeHeader := range []string{
		"bytes=2-5",
		"bytes=-3",
		"bytes=0-1,3-4",
		"bytes=x-",
		"items=0-",
	} {
		t.Run(rangeHeader, func(t *testing.T) {
			t.Parallel()

			store := &fakeObjectStore{
				newReaderFunc: func(context.Context, string) (objectRead, error) {
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
			response := httptest.NewRecorder()

			BucketProxy{store: store}.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if body := response.Body.String(); body != "cache data" {
				t.Errorf("body = %q, want cache data", body)
			}
			if contentRange := response.Header().Get("Content-Range"); contentRange != "" {
				t.Errorf("Content-Range = %q, want empty", contentRange)
			}
		})
	}
}

func TestBucketProxyIgnoresRangeWithIfRange(t *testing.T) {
	t.Parallel()

	store := &fakeObjectStore{
		newReaderFunc: func(context.Context, string) (objectRead, error) {
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
	request.Header.Set("Range", "bytes=6-")
	request.Header.Set("If-Range", `"unsupported-validator"`)
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); body != "cache data" {
		t.Errorf("body = %q, want cache data", body)
	}
}

func TestBucketProxyReportsUnsatisfiableGCSRange(t *testing.T) {
	t.Parallel()

	store := &fakeObjectStore{
		newRangeReaderFunc: func(
			context.Context,
			string,
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
	request.Header.Set("Range", "bytes=2-")
	response := httptest.NewRecorder()

	BucketProxy{store: store}.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); body != "cache data" {
		t.Errorf("body = %q, want cache data", body)
	}
	if length := response.Header().Get("Content-Length"); length != "10" {
		t.Errorf("Content-Length = %q, want 10", length)
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

func assertObjectHeaders(t *testing.T, header http.Header) {
	t.Helper()

	expected := map[string]string{
		"Content-Length":   "10",
		"Content-Type":     "application/octet-stream",
		"Content-Encoding": "zstd",
		"Cache-Control":    "public, max-age=3600",
		"Accept-Ranges":    "bytes",
	}

	for name, want := range expected {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	for _, name := range []string{
		"ETag",
		"Last-Modified",
		"Content-Language",
		"Content-Disposition",
	} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
	}
}

type fakeObjectStore struct {
	attributesFunc     func(context.Context, string) (objectMetadata, error)
	newReaderFunc      func(context.Context, string) (objectRead, error)
	newRangeReaderFunc func(context.Context, string, int64) (objectRead, error)
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
	offset int64,
) (objectRead, error) {
	if s.newRangeReaderFunc == nil {
		panic("unexpected newRangeReader call")
	}

	return s.newRangeReaderFunc(ctx, objectName, offset)
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

const benchmarkObjectSize = 4 * 1024 * 1024

func BenchmarkObjectCopy(b *testing.B) {
	data := make([]byte, benchmarkObjectSize)
	destination := writerOnly{Writer: io.Discard}

	b.Run("io.Copy", func(b *testing.B) {
		b.SetBytes(benchmarkObjectSize)
		b.ReportAllocs()

		for b.Loop() {
			source := readerOnly{Reader: bytes.NewReader(data)}
			if _, err := io.Copy(destination, source); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pooled buffer", func(b *testing.B) {
		b.SetBytes(benchmarkObjectSize)
		b.ReportAllocs()

		for b.Loop() {
			source := bytes.NewReader(data)
			if _, err := copyStream(destination, source); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type writerOnly struct {
	io.Writer
}
