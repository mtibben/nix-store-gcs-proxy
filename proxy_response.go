package main

import (
	"net/http"
	"strconv"
)

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
