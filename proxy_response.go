package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func setObjectResponseHeaders(
	header http.Header,
	metadata objectMetadata,
	contentLength int64,
) {
	setHeaderIfPresent(header, "Content-Type", metadata.contentType)
	setHeaderIfPresent(header, "Content-Language", metadata.contentLanguage)
	if !metadata.decompressed {
		setHeaderIfPresent(header, "Content-Encoding", metadata.contentEncoding)
	}
	setHeaderIfPresent(header, "Content-Disposition", metadata.contentDisposition)
	setHeaderIfPresent(header, "Cache-Control", metadata.cacheControl)

	if contentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	if !metadata.lastModified.IsZero() {
		header.Set("Last-Modified", metadata.lastModified.UTC().Format(http.TimeFormat))
	}
	if etag := objectETag(metadata); etag != "" {
		header.Set("ETag", etag)
	}
}

func setHeaderIfPresent(header http.Header, name, value string) {
	if value != "" {
		header.Set(name, value)
	}
}

func objectETag(metadata objectMetadata) string {
	if metadata.generation != 0 {
		return fmt.Sprintf(
			`"gcs-%d-%d"`,
			metadata.generation,
			metadata.metageneration,
		)
	}

	if metadata.etag == "" {
		return ""
	}
	if strings.HasPrefix(metadata.etag, `"`) || strings.HasPrefix(metadata.etag, "W/") {
		return metadata.etag
	}

	return strconv.Quote(metadata.etag)
}
