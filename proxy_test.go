package main

import (
	"context"
	"io"
	"net/http"
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
	newWriterFunc      func(context.Context, string, objectWriteOptions) io.WriteCloser
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
) io.WriteCloser {
	if s.newWriterFunc == nil {
		panic("unexpected newWriter call")
	}

	return s.newWriterFunc(ctx, objectName, options)
}

var _ objectStore = (*fakeObjectStore)(nil)
