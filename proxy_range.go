package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

var errRangeNotSatisfiable = errors.New("range not satisfiable")

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
