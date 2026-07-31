package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

type objectMetadata struct {
	size               int64
	contentType        string
	contentLanguage    string
	contentEncoding    string
	contentDisposition string
	cacheControl       string
	lastModified       time.Time
	generation         int64
	metageneration     int64
	etag               string
	decompressed       bool
}

type objectRead struct {
	body          io.ReadCloser
	metadata      objectMetadata
	contentLength int64
	startOffset   int64
}

type objectWriteOptions struct {
	contentType        string
	contentLanguage    string
	contentEncoding    string
	contentDisposition string
	cacheControl       string
	chunkSize          int
}

type objectStore interface {
	attributes(context.Context, string) (objectMetadata, error)
	newReader(context.Context, string) (objectRead, error)
	newRangeReader(context.Context, string, int64, int64) (objectRead, error)
	newWriter(context.Context, string, objectWriteOptions) objectWriter
}

type objectWriter interface {
	io.WriteCloser
	abort()
}

type gcsObjectStore struct {
	bucket *storage.BucketHandle
}

var _ objectStore = (*gcsObjectStore)(nil)

func newGCSObjectStore(bucket *storage.BucketHandle) *gcsObjectStore {
	return &gcsObjectStore{bucket: bucket}
}

func (s *gcsObjectStore) attributes(
	ctx context.Context,
	objectName string,
) (objectMetadata, error) {
	attrs, err := s.bucket.Object(objectName).Attrs(ctx)
	if err != nil {
		return objectMetadata{}, fmt.Errorf("read object %q metadata: %w", objectName, err)
	}

	return objectMetadata{
		size:               attrs.Size,
		contentType:        attrs.ContentType,
		contentLanguage:    attrs.ContentLanguage,
		contentEncoding:    attrs.ContentEncoding,
		contentDisposition: attrs.ContentDisposition,
		cacheControl:       attrs.CacheControl,
		lastModified:       attrs.Updated,
		generation:         attrs.Generation,
		metageneration:     attrs.Metageneration,
		etag:               attrs.Etag,
	}, nil
}

func (s *gcsObjectStore) newReader(
	ctx context.Context,
	objectName string,
) (objectRead, error) {
	reader, err := s.bucket.Object(objectName).NewReader(ctx)
	if err != nil {
		return objectRead{}, fmt.Errorf("open object %q: %w", objectName, err)
	}

	return objectReadFromGCSReader(reader), nil
}

func (s *gcsObjectStore) newRangeReader(
	ctx context.Context,
	objectName string,
	offset, length int64,
) (objectRead, error) {
	reader, err := s.bucket.Object(objectName).NewRangeReader(ctx, offset, length)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) &&
			apiErr.Code == http.StatusRequestedRangeNotSatisfiable {
			return objectRead{}, errors.Join(
				errRangeNotSatisfiable,
				fmt.Errorf("open object %q range: %w", objectName, err),
			)
		}
		return objectRead{}, fmt.Errorf("open object %q range: %w", objectName, err)
	}

	return objectReadFromGCSReader(reader), nil
}

func (s *gcsObjectStore) newWriter(
	ctx context.Context,
	objectName string,
	options objectWriteOptions,
) objectWriter {
	writerContext, cancelWriter := context.WithCancel(ctx)
	writer := s.bucket.Object(objectName).NewWriter(writerContext)
	writer.ContentType = options.contentType
	writer.ContentLanguage = options.contentLanguage
	writer.ContentEncoding = options.contentEncoding
	writer.ContentDisposition = options.contentDisposition
	writer.CacheControl = options.cacheControl
	if options.chunkSize > 0 {
		writer.ChunkSize = options.chunkSize
	}

	return &gcsObjectWriter{
		objectName:   objectName,
		writer:       writer,
		cancelWriter: cancelWriter,
	}
}

func objectReadFromGCSReader(reader *storage.Reader) objectRead {
	return objectRead{
		body: reader,
		metadata: objectMetadata{
			size:            reader.Attrs.Size,
			contentType:     reader.Attrs.ContentType,
			contentEncoding: reader.Attrs.ContentEncoding,
			cacheControl:    reader.Attrs.CacheControl,
			lastModified:    reader.Attrs.LastModified,
			generation:      reader.Attrs.Generation,
			metageneration:  reader.Attrs.Metageneration,
			decompressed:    reader.Attrs.Decompressed,
		},
		contentLength: reader.Remain(),
		startOffset:   reader.Attrs.StartOffset,
	}
}

type gcsObjectWriter struct {
	objectName   string
	writer       *storage.Writer
	cancelWriter context.CancelFunc
}

func (w *gcsObjectWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if err != nil {
		return written, fmt.Errorf("write object %q: %w", w.objectName, err)
	}

	return written, nil
}

func (w *gcsObjectWriter) Close() error {
	defer w.cancelWriter()

	if err := w.writer.Close(); err != nil {
		return fmt.Errorf("close object %q writer: %w", w.objectName, err)
	}

	return nil
}

func (w *gcsObjectWriter) abort() {
	w.cancelWriter()
}
