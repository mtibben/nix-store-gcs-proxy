package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/storage"
)

const (
	cacheInfoObjectName     = "nix-cache-info"
	healthReadinessCacheTTL = time.Second
	healthReadinessTimeout  = 5 * time.Second
	livenessEndpointPath    = "/livez"
	readinessEndpointPath   = "/readyz"
	healthyResponseBody     = "ok\n"
	unhealthyResponseBody   = "not ready\n"
	healthResponseCacheMode = "no-store"
)

type readinessCheck func(context.Context) error

type readinessResultCache struct {
	check readinessCheck
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	hasResult bool
	result    error
	expiresAt time.Time
	inFlight  chan struct{}
}

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

func cacheReadinessCheck(check readinessCheck, ttl time.Duration) readinessCheck {
	cache := &readinessResultCache{
		check: check,
		ttl:   ttl,
		now:   time.Now,
	}

	return cache.run
}

func (c *readinessResultCache) run(ctx context.Context) error {
	for {
		now := c.now()

		c.mu.Lock()
		if c.hasResult && now.Before(c.expiresAt) {
			result := c.result
			c.mu.Unlock()
			return result
		}
		if c.inFlight != nil {
			inFlight := c.inFlight
			c.mu.Unlock()

			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for readiness check: %w", ctx.Err())
			case <-inFlight:
				continue
			}
		}

		c.inFlight = make(chan struct{})
		c.mu.Unlock()

		result := c.check(ctx)

		c.mu.Lock()
		c.result = result
		c.hasResult = true
		c.expiresAt = c.now().Add(c.ttl)
		close(c.inFlight)
		c.inFlight = nil
		c.mu.Unlock()

		return result
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
