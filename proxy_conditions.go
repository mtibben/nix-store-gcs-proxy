package main

import (
	"net/http"
	"strings"
	"time"
)

type conditionResult uint8

const (
	conditionNotPresent conditionResult = iota
	conditionTrue
	conditionFalse
)

func readPreconditionStatus(req *http.Request, metadata objectMetadata) int {
	currentETag := objectETag(metadata)

	match := checkIfMatch(req.Header, currentETag)
	if match == conditionNotPresent {
		match = checkIfUnmodifiedSince(req.Header, metadata.lastModified)
	}
	if match == conditionFalse {
		return http.StatusPreconditionFailed
	}

	switch checkIfNoneMatch(req.Header, currentETag) {
	case conditionFalse:
		return http.StatusNotModified
	case conditionNotPresent:
		if checkIfModifiedSince(req.Header, metadata.lastModified) == conditionFalse {
			return http.StatusNotModified
		}
	case conditionTrue:
	}

	return 0
}

func writeReadPreconditionResponse(w http.ResponseWriter, status int) {
	if status == http.StatusNotModified {
		header := w.Header()
		header.Del("Content-Type")
		header.Del("Content-Length")
		header.Del("Content-Encoding")
		if header.Get("ETag") != "" {
			header.Del("Last-Modified")
		}
	}

	w.WriteHeader(status)
}

func checkIfMatch(header http.Header, currentETag string) conditionResult {
	values := header.Values("If-Match")
	if len(values) == 0 {
		return conditionNotPresent
	}

	for value := strings.Join(values, ","); value != ""; {
		value = strings.TrimSpace(value)
		if value == "" {
			break
		}
		if value[0] == ',' {
			value = value[1:]
			continue
		}
		if value[0] == '*' {
			return conditionTrue
		}

		entityTag, remaining := scanEntityTag(value)
		if entityTag == "" {
			break
		}
		if strongETagMatches(entityTag, currentETag) {
			return conditionTrue
		}
		value = remaining
	}

	return conditionFalse
}

func checkIfUnmodifiedSince(header http.Header, lastModified time.Time) conditionResult {
	value := header.Get("If-Unmodified-Since")
	if value == "" || lastModified.IsZero() {
		return conditionNotPresent
	}

	unmodifiedSince, err := http.ParseTime(value)
	if err != nil {
		return conditionNotPresent
	}
	if !lastModified.Truncate(time.Second).After(unmodifiedSince) {
		return conditionTrue
	}

	return conditionFalse
}

func checkIfNoneMatch(header http.Header, currentETag string) conditionResult {
	values := header.Values("If-None-Match")
	if len(values) == 0 {
		return conditionNotPresent
	}

	for value := strings.Join(values, ","); value != ""; {
		value = strings.TrimSpace(value)
		if value == "" {
			break
		}
		if value[0] == ',' {
			value = value[1:]
			continue
		}
		if value[0] == '*' {
			return conditionFalse
		}

		entityTag, remaining := scanEntityTag(value)
		if entityTag == "" {
			break
		}
		if weakETagMatches(entityTag, currentETag) {
			return conditionFalse
		}
		value = remaining
	}

	return conditionTrue
}

func checkIfModifiedSince(header http.Header, lastModified time.Time) conditionResult {
	value := header.Get("If-Modified-Since")
	if value == "" || lastModified.IsZero() {
		return conditionNotPresent
	}

	modifiedSince, err := http.ParseTime(value)
	if err != nil {
		return conditionNotPresent
	}
	if !lastModified.Truncate(time.Second).After(modifiedSince) {
		return conditionFalse
	}

	return conditionTrue
}

func ifRangeMatches(value string, metadata objectMetadata) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}

	entityTag, _ := scanEntityTag(value)
	if entityTag != "" {
		return strongETagMatches(entityTag, objectETag(metadata))
	}
	if metadata.lastModified.IsZero() {
		return false
	}

	validatorTime, err := http.ParseTime(value)
	if err != nil {
		return false
	}

	return metadata.lastModified.Unix() == validatorTime.Unix()
}

func scanEntityTag(value string) (entityTag, remaining string) {
	value = strings.TrimSpace(value)
	tagStart := 0
	if strings.HasPrefix(value, "W/") {
		tagStart = 2
	}
	if len(value[tagStart:]) < 2 || value[tagStart] != '"' {
		return "", ""
	}

	for index := tagStart + 1; index < len(value); index++ {
		character := value[index]
		switch {
		case character == 0x21 ||
			character >= 0x23 && character <= 0x7e ||
			character >= 0x80:
		case character == '"':
			return value[:index+1], value[index+1:]
		default:
			return "", ""
		}
	}

	return "", ""
}

func strongETagMatches(left, right string) bool {
	return left == right && left != "" && left[0] == '"'
}

func weakETagMatches(left, right string) bool {
	return strings.TrimPrefix(left, "W/") == strings.TrimPrefix(right, "W/")
}
