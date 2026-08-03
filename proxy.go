package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
)

const streamCopyBufferSize = 32 * 1024

var errRangeNotSatisfiable = errors.New("range not satisfiable")

var streamCopyBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, streamCopyBufferSize)
		return &buffer
	},
}

type BucketProxy struct {
	store objectStore
}

func (s BucketProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	objectPath := req.URL.Path[1:]

	switch req.Method {
	case http.MethodHead:
		s.serveHead(w, req, objectPath)
	case http.MethodGet:
		s.serveGet(w, req, objectPath)
	case http.MethodPut:
		s.servePut(w, req, objectPath)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		msg := fmt.Sprintf("Method '%s' is not supported", req.Method)
		http.Error(w, msg, http.StatusMethodNotAllowed)
	}
}

func (s BucketProxy) serveHead(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	metadata, err := s.store.attributes(req.Context(), objectPath)
	if err != nil {
		writeObjectReadError(w, err)
		return
	}
	setObjectResponseHeaders(w.Header(), metadata, metadata.size)
	w.Header().Set("Accept-Ranges", "bytes")
}

func (s BucketProxy) serveGet(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	object, partial, err := s.readObject(req, objectPath)
	if err != nil {
		if errors.Is(err, errRangeNotSatisfiable) {
			s.writeRangeError(w, req.Context(), objectPath)
			return
		}
		writeObjectReadError(w, err)
		return
	}
	defer func() {
		if err := object.body.Close(); err != nil {
			log.Println(err)
		}
	}()

	setObjectResponseHeaders(w.Header(), object.metadata, object.contentLength)
	if !object.metadata.decompressed {
		w.Header().Set("Accept-Ranges", "bytes")
		if partial {
			setPartialContentHeaders(w.Header(), object)
			w.WriteHeader(http.StatusPartialContent)
		}
	}
	if _, err := copyStream(w, object.body); err != nil {
		log.Println(err)
	}
}

func (s BucketProxy) readObject(
	req *http.Request,
	objectPath string,
) (objectRead, bool, error) {
	ctx := req.Context()
	rangeHeader := req.Header.Get("Range")
	if rangeHeader == "" || req.Header.Get("If-Range") != "" {
		object, err := s.store.newReader(ctx, objectPath)
		return object, false, err
	}

	offset, supported := parseNixResumeRange(rangeHeader)
	if !supported {
		object, err := s.store.newReader(ctx, objectPath)
		return object, false, err
	}

	object, err := s.store.newRangeReader(ctx, objectPath, offset)
	return object, true, err
}

func (s BucketProxy) writeRangeError(
	w http.ResponseWriter,
	ctx context.Context,
	objectPath string,
) {
	metadata, err := s.store.attributes(ctx, objectPath)
	if err != nil {
		writeObjectReadError(w, err)
		return
	}

	writeRangeNotSatisfiable(w, metadata.size)
}

func writeObjectReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrObjectNotExist) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	http.Error(w, err.Error(), http.StatusBadGateway)
}

func parseNixResumeRange(value string) (int64, bool) {
	unit, value, ok := strings.Cut(value, "=")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return 0, false
	}

	value = strings.TrimSpace(value)
	startValue, endValue, ok := strings.Cut(value, "-")
	if !ok || strings.TrimSpace(endValue) != "" {
		return 0, false
	}

	startValue = strings.TrimSpace(startValue)
	if startValue == "" {
		return 0, false
	}

	offset, err := strconv.ParseInt(startValue, 10, 64)
	if err != nil || offset < 0 {
		return 0, false
	}

	return offset, true
}

func setObjectResponseHeaders(
	header http.Header,
	metadata objectMetadata,
	contentLength int64,
) {
	setHeaderIfPresent(header, "Content-Type", metadata.contentType)
	if !metadata.decompressed {
		setHeaderIfPresent(header, "Content-Encoding", metadata.contentEncoding)
	}
	setHeaderIfPresent(header, "Cache-Control", metadata.cacheControl)

	if contentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
}

func setHeaderIfPresent(header http.Header, name, value string) {
	if value != "" {
		header.Set(name, value)
	}
}

func setPartialContentHeaders(header http.Header, object objectRead) {
	endOffset := object.startOffset + object.contentLength - 1
	header.Set(
		"Content-Range",
		fmt.Sprintf(
			"bytes %d-%d/%d",
			object.startOffset,
			endOffset,
			object.metadata.size,
		),
	)
}

func writeRangeNotSatisfiable(w http.ResponseWriter, objectSize int64) {
	w.Header().Set("Accept-Ranges", "bytes")
	if objectSize >= 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", objectSize))
	}
	http.Error(
		w,
		http.StatusText(http.StatusRequestedRangeNotSatisfiable),
		http.StatusRequestedRangeNotSatisfiable,
	)
}

func copyStream(destination io.Writer, source io.Reader) (int64, error) {
	value := streamCopyBufferPool.Get()
	buffer, ok := value.(*[]byte)
	if !ok {
		panic("stream copy buffer pool returned an unexpected value")
	}
	defer streamCopyBufferPool.Put(buffer)

	written, err := io.CopyBuffer(destination, readerOnly{Reader: source}, *buffer)
	if err != nil {
		return written, fmt.Errorf("copy stream: %w", err)
	}

	return written, nil
}

type readerOnly struct {
	io.Reader
}
