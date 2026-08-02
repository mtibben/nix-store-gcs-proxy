package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func TestGCSObjectStorePreservesMetadataAndStoredEncoding(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "cache"
		objectName = "example.nar"
		content    = "cache data"
	)
	compressed := gzipContent(t, content)
	var downloadRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/"+bucketName+"/"+objectName {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}

		downloadRequests.Add(1)
		if encoding := req.Header.Get("Accept-Encoding"); encoding != "gzip" {
			t.Errorf("Accept-Encoding = %q, want gzip", encoding)
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if _, err := w.Write(compressed); err != nil {
			t.Errorf("write object response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	store := newTestGCSObjectStore(t, server, bucketName)
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

	assertStoredObjectMetadata(t, object.metadata, int64(len(compressed)))
	if got := downloadRequests.Load(); got != 1 {
		t.Errorf("download requests = %d, want 1", got)
	}
}

func TestGCSObjectStoreUsesNixResumeRange(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "cache"
		objectName = "example.nar"
	)
	var downloadRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/"+bucketName+"/"+objectName {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}

		downloadRequests.Add(1)
		if encoding := req.Header.Get("Accept-Encoding"); encoding != "gzip" {
			t.Errorf("Accept-Encoding = %q, want gzip", encoding)
		}

		switch byteRange := req.Header.Get("Range"); byteRange {
		case "bytes=6-":
			w.Header().Set("Content-Type", "application/x-nix-nar")
			w.Header().Set("Content-Encoding", "zstd")
			w.Header().Set("Content-Length", "4")
			w.Header().Set("Content-Range", "bytes 6-9/10")
			w.WriteHeader(http.StatusPartialContent)
			if _, err := io.WriteString(w, "6789"); err != nil {
				t.Errorf("write range response: %v", err)
			}
		case "bytes=20-":
			http.Error(
				w,
				http.StatusText(http.StatusRequestedRangeNotSatisfiable),
				http.StatusRequestedRangeNotSatisfiable,
			)
		default:
			http.Error(w, "unexpected range", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	store := newTestGCSObjectStore(t, server, bucketName)
	object, err := store.newRangeReader(context.Background(), objectName, 6)
	if err != nil {
		t.Fatalf("open object range: %v", err)
	}
	body, readErr := io.ReadAll(object.body)
	closeErr := object.body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("read object range: %v", err)
	}
	if got := string(body); got != "6789" {
		t.Errorf("body = %q, want 6789", got)
	}
	if object.metadata.size != 10 {
		t.Errorf("size = %d, want 10", object.metadata.size)
	}
	if object.metadata.contentEncoding != "zstd" {
		t.Errorf("content encoding = %q, want zstd", object.metadata.contentEncoding)
	}
	if object.startOffset != 6 {
		t.Errorf("start offset = %d, want 6", object.startOffset)
	}
	if object.contentLength != 4 {
		t.Errorf("content length = %d, want 4", object.contentLength)
	}

	_, err = store.newRangeReader(context.Background(), objectName, 20)
	if !errors.Is(err, errRangeNotSatisfiable) {
		t.Errorf("error = %v, want errRangeNotSatisfiable", err)
	}
	if got := downloadRequests.Load(); got != 2 {
		t.Errorf("download requests = %d, want 2", got)
	}
}

func TestGCSObjectStoreCreatesObjectsOnly(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "cache"
		objectName = "example.narinfo"
	)
	var uploadRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		uploadRequests.Add(1)
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if req.URL.Path != "/upload/storage/v1/b/"+bucketName+"/o" {
			t.Errorf(
				"path = %q, want /upload/storage/v1/b/%s/o",
				req.URL.Path,
				bucketName,
			)
		}
		if got := req.URL.Query().Get("name"); got != objectName {
			t.Errorf("object name = %q, want %q", got, objectName)
		}
		if got := req.URL.Query().Get("ifGenerationMatch"); got != "0" {
			t.Errorf("ifGenerationMatch = %q, want 0", got)
		}

		http.Error(w, "object exists", http.StatusPreconditionFailed)
	}))
	t.Cleanup(server.Close)

	store := newTestGCSObjectStore(t, server, bucketName)
	writer := store.newWriter(
		context.Background(),
		objectName,
		objectWriteOptions{contentType: "text/x-nix-narinfo"},
	)
	_, writeErr := writer.Write([]byte("cache data"))
	closeErr := writer.Close()
	if err := errors.Join(writeErr, closeErr); !errors.Is(err, errObjectAlreadyExists) {
		t.Errorf("error = %v, want errObjectAlreadyExists", err)
	}
	if got := uploadRequests.Load(); got != 1 {
		t.Errorf("upload requests = %d, want 1", got)
	}
}

func TestClassifyObjectWriteErrorMarksCreateOnlyConflicts(t *testing.T) {
	t.Parallel()

	apiErr := &googleapi.Error{Code: http.StatusPreconditionFailed}
	wrapped := fmt.Errorf("close object writer: %w", apiErr)
	err := classifyObjectWriteError(wrapped)

	if !errors.Is(err, errObjectAlreadyExists) {
		t.Errorf("error = %v, want errObjectAlreadyExists", err)
	}
	if !errors.Is(err, apiErr) {
		t.Errorf("error = %v, want wrapped API error", err)
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
) {
	t.Helper()

	if metadata.size != size {
		t.Errorf("size = %d, want %d", metadata.size, size)
	}
	if metadata.contentType != "application/octet-stream" {
		t.Errorf("content type = %q, want application/octet-stream", metadata.contentType)
	}
	if metadata.contentEncoding != "gzip" {
		t.Errorf("content encoding = %q, want gzip", metadata.contentEncoding)
	}
	if metadata.cacheControl != "public, max-age=3600" {
		t.Errorf(
			"cache control = %q, want public, max-age=3600",
			metadata.cacheControl,
		)
	}
}

func newTestGCSObjectStore(
	t *testing.T,
	server *httptest.Server,
	bucketName string,
) *gcsObjectStore {
	t.Helper()

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

	return newGCSObjectStore(client.Bucket(bucketName))
}
