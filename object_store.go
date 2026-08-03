package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

type objectMetadata struct {
	size            int64
	contentType     string
	contentEncoding string
	cacheControl    string
	decompressed    bool
}

type objectRead struct {
	body          io.ReadCloser
	metadata      objectMetadata
	contentLength int64
	startOffset   int64
}

type objectWriteOptions struct {
	contentType     string
	contentEncoding string
	cacheControl    string
	chunkSize       int
}

type objectStore interface {
	attributes(context.Context, string) (objectMetadata, error)
	newReader(context.Context, string) (objectRead, error)
	newRangeReader(context.Context, string, int64) (objectRead, error)
	newWriter(context.Context, string, objectWriteOptions) objectWriter
}

type objectWriter interface {
	io.WriteCloser
	abort()
}

type gcsObjectStore struct {
	bucket *storage.BucketHandle
}

var errObjectAlreadyExists = errors.New("object already exists")

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

	return objectMetadataFromGCSAttrs(attrs), nil
}

func objectMetadataFromGCSAttrs(attrs *storage.ObjectAttrs) objectMetadata {
	return objectMetadata{
		size:            attrs.Size,
		contentType:     attrs.ContentType,
		contentEncoding: attrs.ContentEncoding,
		cacheControl:    attrs.CacheControl,
	}
}

func (s *gcsObjectStore) newReader(
	ctx context.Context,
	objectName string,
) (objectRead, error) {
	reader, err := s.bucket.Object(objectName).ReadCompressed(true).NewReader(ctx)
	if err != nil {
		return objectRead{}, fmt.Errorf("open object %q: %w", objectName, err)
	}

	return objectReadFromGCSReader(reader), nil
}

func (s *gcsObjectStore) newRangeReader(
	ctx context.Context,
	objectName string,
	offset int64,
) (objectRead, error) {
	reader, err := s.bucket.Object(objectName).ReadCompressed(true).NewRangeReader(
		ctx,
		offset,
		-1,
	)
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
	object := s.bucket.Object(objectName).If(storage.Conditions{DoesNotExist: true})
	writer := object.NewWriter(writerContext)
	writer.ContentType = options.contentType
	writer.ContentEncoding = options.contentEncoding
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
		return written, classifyObjectWriteError(
			fmt.Errorf("write object %q: %w", w.objectName, err),
		)
	}

	return written, nil
}

func (w *gcsObjectWriter) Close() error {
	defer w.cancelWriter()

	if err := w.writer.Close(); err != nil {
		return classifyObjectWriteError(
			fmt.Errorf("close object %q writer: %w", w.objectName, err),
		)
	}

	return nil
}

func (w *gcsObjectWriter) abort() {
	w.cancelWriter()
}

func classifyObjectWriteError(err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
		return errors.Join(errObjectAlreadyExists, err)
	}

	return err
}
