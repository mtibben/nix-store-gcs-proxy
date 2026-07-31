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
	conditions         objectWriteConditions
	reuseExisting      bool
}

type objectWriteConditions struct {
	doesNotExist        bool
	generationMatch     int64
	metagenerationMatch int64
}

func (c objectWriteConditions) toGCSConditions() storage.Conditions {
	return storage.Conditions{
		DoesNotExist:        c.doesNotExist,
		GenerationMatch:     c.generationMatch,
		MetagenerationMatch: c.metagenerationMatch,
	}
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

var (
	errObjectAlreadyExists   = errors.New("object already exists")
	errObjectContentConflict = errors.New("object already exists with different content")
)

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
	}
}

func (s *gcsObjectStore) newReader(
	ctx context.Context,
	objectName string,
) (objectRead, error) {
	object, metadata, err := s.objectForRead(ctx, objectName)
	if err != nil {
		return objectRead{}, err
	}

	reader, err := object.NewReader(ctx)
	if err != nil {
		return objectRead{}, fmt.Errorf("open object %q: %w", objectName, err)
	}

	return objectReadFromGCSReader(reader, metadata), nil
}

func (s *gcsObjectStore) newRangeReader(
	ctx context.Context,
	objectName string,
	offset, length int64,
) (objectRead, error) {
	object, metadata, err := s.objectForRead(ctx, objectName)
	if err != nil {
		return objectRead{}, err
	}

	reader, err := object.NewRangeReader(ctx, offset, length)
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

	return objectReadFromGCSReader(reader, metadata), nil
}

func (s *gcsObjectStore) objectForRead(
	ctx context.Context,
	objectName string,
) (*storage.ObjectHandle, objectMetadata, error) {
	object := s.bucket.Object(objectName)
	attrs, err := object.Attrs(ctx)
	if err != nil {
		return nil, objectMetadata{}, fmt.Errorf(
			"read object %q metadata: %w",
			objectName,
			err,
		)
	}

	metadata := objectMetadataFromGCSAttrs(attrs)
	object = object.If(storage.Conditions{
		GenerationMatch:     attrs.Generation,
		MetagenerationMatch: attrs.Metageneration,
	}).ReadCompressed(true)

	return object, metadata, nil
}

func (s *gcsObjectStore) newWriter(
	ctx context.Context,
	objectName string,
	options objectWriteOptions,
) objectWriter {
	writerContext, cancelWriter := context.WithCancel(ctx)
	object := s.bucket.Object(objectName)
	if options.conditions != (objectWriteConditions{}) {
		object = object.If(options.conditions.toGCSConditions())
	}
	writer := object.NewWriter(writerContext)
	writer.ContentType = options.contentType
	writer.ContentLanguage = options.contentLanguage
	writer.ContentEncoding = options.contentEncoding
	writer.ContentDisposition = options.contentDisposition
	writer.CacheControl = options.cacheControl
	if options.chunkSize > 0 {
		writer.ChunkSize = options.chunkSize
	}

	preconditionError := errObjectPreconditionFailed
	if options.conditions.doesNotExist && options.reuseExisting {
		preconditionError = errObjectAlreadyExists
	}

	return &gcsObjectWriter{
		objectName:        objectName,
		writer:            writer,
		cancelWriter:      cancelWriter,
		preconditionError: preconditionError,
	}
}

func objectReadFromGCSReader(
	reader *storage.Reader,
	metadata objectMetadata,
) objectRead {
	metadata.decompressed = reader.Attrs.Decompressed

	return objectRead{
		body:          reader,
		metadata:      metadata,
		contentLength: reader.Remain(),
		startOffset:   reader.Attrs.StartOffset,
	}
}

type gcsObjectWriter struct {
	objectName        string
	writer            *storage.Writer
	cancelWriter      context.CancelFunc
	preconditionError error
}

var errObjectPreconditionFailed = errors.New("object precondition failed")

func (w *gcsObjectWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if err != nil {
		return written, classifyObjectWriteErrorAs(
			fmt.Errorf("write object %q: %w", w.objectName, err),
			w.preconditionError,
		)
	}

	return written, nil
}

func (w *gcsObjectWriter) Close() error {
	defer w.cancelWriter()

	if err := w.writer.Close(); err != nil {
		return classifyObjectWriteErrorAs(
			fmt.Errorf("close object %q writer: %w", w.objectName, err),
			w.preconditionError,
		)
	}

	return nil
}

func (w *gcsObjectWriter) abort() {
	w.cancelWriter()
}

func classifyObjectWriteError(err error) error {
	return classifyObjectWriteErrorAs(err, errObjectPreconditionFailed)
}

func classifyObjectWriteErrorAs(err, preconditionError error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
		return errors.Join(preconditionError, err)
	}

	return err
}
