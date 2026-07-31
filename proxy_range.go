package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

var errRangeNotSatisfiable = errors.New("range not satisfiable")
var errInvalidByteRange = fmt.Errorf("%w: invalid byte range", errRangeNotSatisfiable)

type objectByteRange struct {
	offset int64
	length int64
}

func parseObjectByteRange(value string) (objectByteRange, error) {
	const rangePrefix = "bytes="

	if !strings.HasPrefix(value, rangePrefix) {
		return objectByteRange{}, fmt.Errorf(
			"%w: only byte ranges are supported",
			errInvalidByteRange,
		)
	}

	value = strings.TrimSpace(strings.TrimPrefix(value, rangePrefix))
	if value == "" || strings.Contains(value, ",") {
		return objectByteRange{}, fmt.Errorf(
			"%w: exactly one byte range is required",
			errInvalidByteRange,
		)
	}

	startValue, endValue, ok := strings.Cut(value, "-")
	if !ok {
		return objectByteRange{}, fmt.Errorf(
			"%w: missing range separator",
			errInvalidByteRange,
		)
	}

	startValue = strings.TrimSpace(startValue)
	endValue = strings.TrimSpace(endValue)
	if startValue == "" {
		return parseSuffixByteRange(endValue)
	}

	start, err := parseNonNegativeRangeNumber(startValue)
	if err != nil {
		return objectByteRange{}, err
	}
	if endValue == "" {
		return objectByteRange{offset: start, length: -1}, nil
	}

	end, err := parseNonNegativeRangeNumber(endValue)
	if err != nil {
		return objectByteRange{}, err
	}
	if end < start || end-start == math.MaxInt64 {
		return objectByteRange{}, fmt.Errorf(
			"%w: range end precedes range start",
			errInvalidByteRange,
		)
	}

	return objectByteRange{
		offset: start,
		length: end - start + 1,
	}, nil
}

func parseSuffixByteRange(value string) (objectByteRange, error) {
	length, err := parseNonNegativeRangeNumber(value)
	if err != nil {
		return objectByteRange{}, err
	}
	if length == 0 {
		return objectByteRange{}, fmt.Errorf(
			"%w: suffix length must be positive",
			errInvalidByteRange,
		)
	}

	return objectByteRange{offset: -length, length: -1}, nil
}

func parseNonNegativeRangeNumber(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("%w: empty range value", errInvalidByteRange)
	}

	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf(
			"%w: invalid range value %q",
			errInvalidByteRange,
			value,
		)
	}

	return number, nil
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
