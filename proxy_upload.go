package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
)

const (
	gcsUploadChunkAlignment = 256 * 1024
	gcsDefaultUploadChunk   = 16 * 1024 * 1024
)

var errObjectContentConflict = errors.New("object already exists with different content")

type uploadResult struct {
	size          int64
	digest        []byte
	alreadyExists bool
}

type uploadExpectation struct {
	size    int64
	digest  []byte
	options objectWriteOptions
}

func (s BucketProxy) servePut(
	w http.ResponseWriter,
	req *http.Request,
	objectPath string,
) {
	ctx := req.Context()
	options := writeOptionsFromRequest(req)
	writer := s.store.newWriter(ctx, objectPath, options)
	upload, err := streamUpload(objectPath, writer, req.Body)
	if err != nil {
		writeObjectWriteError(w, err)
		return
	}
	if upload.alreadyExists {
		want := uploadExpectation{
			size:    upload.size,
			digest:  upload.digest,
			options: options,
		}
		s.writeExistingUploadResponse(w, ctx, objectPath, want)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeUploadSuccess(w)
}

func streamUpload(
	objectPath string,
	wc objectWriter,
	source io.Reader,
) (uploadResult, error) {
	digest := sizedHash{Hash: sha256.New()}
	bodyReader := &readErrorRecorder{reader: source}
	body := io.TeeReader(bodyReader, &digest)
	writer := &writeErrorRecorder{writer: wc}

	_, copyErr := copyStream(writer, body)
	if copyErr == nil {
		closeErr := wc.Close()
		return completedUploadResult(&digest, closeErr)
	}

	if bodyReader.err != nil {
		wc.abort()
		return uploadResult{}, fmt.Errorf(
			"stream upload %q: %w",
			objectPath,
			bodyReader.err,
		)
	}
	if writer.err == nil {
		wc.abort()
		return uploadResult{}, fmt.Errorf(
			"stream upload %q: %w",
			objectPath,
			copyErr,
		)
	}

	uploadErr := fmt.Errorf("stream upload %q: %w", objectPath, copyErr)
	uploadErr = errors.Join(uploadErr, wc.Close())
	if !errors.Is(uploadErr, errObjectAlreadyExists) {
		return uploadResult{}, uploadErr
	}

	if _, err := copyStream(io.Discard, body); err != nil {
		return uploadResult{}, fmt.Errorf(
			"finish hashing upload %q: %w",
			objectPath,
			err,
		)
	}

	return uploadResult{
		size:          digest.size,
		digest:        digest.Sum(nil),
		alreadyExists: true,
	}, nil
}

func completedUploadResult(digest *sizedHash, closeErr error) (uploadResult, error) {
	result := uploadResult{
		size:   digest.size,
		digest: digest.Sum(nil),
	}
	if closeErr == nil {
		return result, nil
	}
	if errors.Is(closeErr, errObjectAlreadyExists) {
		result.alreadyExists = true
		return result, nil
	}

	return uploadResult{}, closeErr
}

type readErrorRecorder struct {
	reader io.Reader
	err    error
}

func (r *readErrorRecorder) Read(data []byte) (int, error) {
	read, err := r.reader.Read(data)
	if err == nil {
		return read, nil
	}
	if err == io.EOF {
		return read, io.EOF
	}

	wrappedErr := fmt.Errorf("read upload body: %w", err)
	if r.err == nil {
		r.err = wrappedErr
	}
	return read, wrappedErr
}

type sizedHash struct {
	hash.Hash
	size int64
}

func (h *sizedHash) Write(data []byte) (int, error) {
	written, err := h.Hash.Write(data)
	h.size += int64(written)
	if err != nil {
		return written, fmt.Errorf("hash upload: %w", err)
	}
	return written, nil
}

type writeErrorRecorder struct {
	writer io.Writer
	err    error
}

func (w *writeErrorRecorder) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if err != nil {
		err = fmt.Errorf("write upload: %w", err)
	}
	w.err = err
	return written, err
}

func (s BucketProxy) writeExistingUploadResponse(
	w http.ResponseWriter,
	ctx context.Context,
	objectPath string,
	want uploadExpectation,
) {
	identical, err := s.objectMatches(ctx, objectPath, want)
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if identical {
		writeUploadSuccess(w)
		return
	}

	conflictErr := fmt.Errorf("%w: %q", errObjectContentConflict, objectPath)
	log.Println(conflictErr)
	http.Error(w, conflictErr.Error(), http.StatusConflict)
}

func (s BucketProxy) objectMatches(
	ctx context.Context,
	objectPath string,
	want uploadExpectation,
) (bool, error) {
	object, err := s.store.newReader(ctx, objectPath)
	if err != nil {
		return false, fmt.Errorf(
			"read concurrently-created object %q: %w",
			objectPath,
			err,
		)
	}

	digest := sha256.New()
	size, copyErr := copyStream(digest, object.body)
	closeErr := object.body.Close()
	if copyErr != nil {
		return false, fmt.Errorf(
			"hash concurrently-created object %q: %w",
			objectPath,
			copyErr,
		)
	}
	if closeErr != nil {
		return false, fmt.Errorf(
			"close concurrently-created object %q: %w",
			objectPath,
			closeErr,
		)
	}

	return size == want.size &&
		bytes.Equal(digest.Sum(nil), want.digest) &&
		objectMetadataMatchesWriteOptions(object.metadata, want.options), nil
}

func objectMetadataMatchesWriteOptions(
	metadata objectMetadata,
	options objectWriteOptions,
) bool {
	// GCS derives Content-Type and may apply a default Cache-Control value when
	// those request headers are absent, so blank values do not constrain them.
	contentTypeMatches := options.contentType == "" ||
		metadata.contentType == options.contentType
	cacheControlMatches := options.cacheControl == "" ||
		metadata.cacheControl == options.cacheControl

	return contentTypeMatches &&
		metadata.contentEncoding == options.contentEncoding &&
		cacheControlMatches
}

func writeUploadSuccess(w http.ResponseWriter) {
	if _, err := fmt.Fprint(w, "OK"); err != nil {
		log.Println(err)
	}
}

func writeObjectWriteError(w http.ResponseWriter, err error) {
	log.Println(err)
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func writeOptionsFromRequest(req *http.Request) objectWriteOptions {
	return objectWriteOptions{
		contentType:     req.Header.Get("Content-Type"),
		contentEncoding: req.Header.Get("Content-Encoding"),
		cacheControl:    req.Header.Get("Cache-Control"),
		chunkSize:       uploadChunkSize(req.ContentLength),
	}
}

func uploadChunkSize(contentLength int64) int {
	if contentLength < 0 || contentLength >= gcsDefaultUploadChunk {
		return gcsDefaultUploadChunk
	}

	sizeWithFinalByte := contentLength + 1
	alignedSize := (sizeWithFinalByte + gcsUploadChunkAlignment - 1) /
		gcsUploadChunkAlignment *
		gcsUploadChunkAlignment

	return int(alignedSize)
}
