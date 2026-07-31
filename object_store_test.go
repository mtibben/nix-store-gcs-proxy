package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

func TestGCSObjectStorePreservesMetadataAndStoredEncoding(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "cache"
		objectName = "example.nar"
		content    = "cache data"
	)
	modified := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	compressed := gzipContent(t, content)
	var metadataRequests atomic.Int32
	var downloadRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Query().Get("alt") {
		case "json":
			metadataRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, err := fmt.Fprintf(
				w,
				`{
					"bucket": %q,
					"name": %q,
					"size": %q,
					"contentType": "application/octet-stream",
					"contentLanguage": "en",
					"contentEncoding": "gzip",
					"contentDisposition": "attachment",
					"cacheControl": "public, max-age=3600",
					"generation": "42",
					"metageneration": "3",
					"etag": "gcs-etag",
					"updated": %q
				}`,
				bucketName,
				objectName,
				strconv.Itoa(len(compressed)),
				modified.Format(time.RFC3339),
			)
			if err != nil {
				t.Errorf("write metadata response: %v", err)
			}
		default:
			if req.URL.Path != "/"+bucketName+"/"+objectName {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}

			downloadRequests.Add(1)
			if encoding := req.Header.Get("Accept-Encoding"); encoding != "gzip" {
				t.Errorf("Accept-Encoding = %q, want gzip", encoding)
			}
			if generation := req.Header.Get("X-Goog-If-Generation-Match"); generation != "42" {
				t.Errorf("X-Goog-If-Generation-Match = %q, want 42", generation)
			}
			if metageneration := req.Header.Get("X-Goog-If-Metageneration-Match"); metageneration != "3" {
				t.Errorf("X-Goog-If-Metageneration-Match = %q, want 3", metageneration)
			}

			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
			w.Header().Set("X-Goog-Generation", "42")
			w.Header().Set("X-Goog-Metageneration", "3")
			if _, err := w.Write(compressed); err != nil {
				t.Errorf("write object response: %v", err)
			}
		}
	}))
	t.Cleanup(server.Close)

	client, err := storage.NewClient(
		context.Background(),
		option.WithEndpoint(server.URL),
		option.WithoutAuthentication(),
		option.WithHTTPClient(server.Client()),
	)
	if err != nil {
		t.Fatalf("create storage client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close storage client: %v", err)
		}
	})

	store := newGCSObjectStore(client.Bucket(bucketName))
	object, err := store.newReader(context.Background(), objectName)
	if err != nil {
		t.Fatalf("open object: %v", err)
	}
	defer func() {
		if err := object.body.Close(); err != nil {
			t.Errorf("close object: %v", err)
		}
	}()

	body, err := io.ReadAll(object.body)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(body, compressed) {
		t.Errorf("body = %x, want stored gzip bytes %x", body, compressed)
	}
	if object.contentLength != int64(len(compressed)) {
		t.Errorf(
			"content length = %d, want %d",
			object.contentLength,
			len(compressed),
		)
	}
	if object.metadata.decompressed {
		t.Error("object was transparently decompressed")
	}

	assertStoredObjectMetadata(t, object.metadata, int64(len(compressed)), modified)
	if got := metadataRequests.Load(); got != 1 {
		t.Errorf("metadata requests = %d, want 1", got)
	}
	if got := downloadRequests.Load(); got != 1 {
		t.Errorf("download requests = %d, want 1", got)
	}
}

func gzipContent(t *testing.T, content string) []byte {
	t.Helper()

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("compress content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}

	return compressed.Bytes()
}

func assertStoredObjectMetadata(
	t *testing.T,
	metadata objectMetadata,
	size int64,
	modified time.Time,
) {
	t.Helper()

	if metadata.size != size {
		t.Errorf("size = %d, want %d", metadata.size, size)
	}
	if metadata.contentType != "application/octet-stream" {
		t.Errorf("content type = %q, want application/octet-stream", metadata.contentType)
	}
	if metadata.contentLanguage != "en" {
		t.Errorf("content language = %q, want en", metadata.contentLanguage)
	}
	if metadata.contentEncoding != "gzip" {
		t.Errorf("content encoding = %q, want gzip", metadata.contentEncoding)
	}
	if metadata.contentDisposition != "attachment" {
		t.Errorf("content disposition = %q, want attachment", metadata.contentDisposition)
	}
	if metadata.cacheControl != "public, max-age=3600" {
		t.Errorf(
			"cache control = %q, want public, max-age=3600",
			metadata.cacheControl,
		)
	}
	if !metadata.lastModified.Equal(modified) {
		t.Errorf("last modified = %v, want %v", metadata.lastModified, modified)
	}
	if metadata.generation != 42 {
		t.Errorf("generation = %d, want 42", metadata.generation)
	}
	if metadata.metageneration != 3 {
		t.Errorf("metageneration = %d, want 3", metadata.metageneration)
	}
	if metadata.etag != "gcs-etag" {
		t.Errorf("etag = %q, want gcs-etag", metadata.etag)
	}
}
