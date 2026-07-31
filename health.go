package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"cloud.google.com/go/storage"
)

const (
	cacheInfoObjectName     = "nix-cache-info"
	healthReadinessTimeout  = 5 * time.Second
	livenessEndpointPath    = "/livez"
	readinessEndpointPath   = "/readyz"
	healthyResponseBody     = "ok\n"
	unhealthyResponseBody   = "not ready\n"
	healthResponseCacheMode = "no-store"
)

type readinessCheck func(context.Context) error

func newHTTPHandler(
	proxy http.Handler,
	check readinessCheck,
	readinessTimeout time.Duration,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+livenessEndpointPath, func(w http.ResponseWriter, req *http.Request) {
		writeHealthResponse(w, req, http.StatusOK, healthyResponseBody)
	})
	mux.HandleFunc("GET "+readinessEndpointPath, func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), readinessTimeout)
		defer cancel()

		if err := check(ctx); err != nil {
			log.Printf("Readiness check failed: %v", err)
			writeHealthResponse(w, req, http.StatusServiceUnavailable, unhealthyResponseBody)
			return
		}

		writeHealthResponse(w, req, http.StatusOK, healthyResponseBody)
	})
	mux.HandleFunc(livenessEndpointPath, rejectHealthMethod)
	mux.HandleFunc(readinessEndpointPath, rejectHealthMethod)
	mux.Handle("/", proxy)

	return mux
}

func newCacheReadinessCheck(bucket *storage.BucketHandle) readinessCheck {
	cacheInfo := bucket.Object(cacheInfoObjectName)

	return func(ctx context.Context) error {
		if _, err := cacheInfo.Attrs(ctx); err != nil {
			return fmt.Errorf("read %s metadata: %w", cacheInfoObjectName, err)
		}

		return nil
	}
}

func rejectHealthMethod(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

func writeHealthResponse(
	w http.ResponseWriter,
	req *http.Request,
	status int,
	body string,
) {
	w.Header().Set("Cache-Control", healthResponseCacheMode)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)

	if req.Method == http.MethodHead {
		return
	}
	if _, err := fmt.Fprint(w, body); err != nil {
		log.Printf("Write health response: %v", err)
	}
}
