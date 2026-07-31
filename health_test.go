package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var errReadinessUnavailable = errors.New("readiness unavailable")

func TestLivenessEndpoint(t *testing.T) {
	t.Parallel()

	readinessCalled := false
	handler := newHTTPHandler(
		http.NotFoundHandler(),
		func(context.Context) error {
			readinessCalled = true
			return nil
		},
		time.Second,
	)

	response := serveRequest(handler, http.MethodGet, livenessEndpointPath)

	assertHealthResponse(t, response, http.StatusOK, healthyResponseBody)
	if readinessCalled {
		t.Error("liveness endpoint called the readiness check")
	}
}

func TestReadinessEndpoint(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		check      readinessCheck
		wantStatus int
		wantBody   string
	}{
		"ready": {
			check: func(context.Context) error {
				return nil
			},
			wantStatus: http.StatusOK,
			wantBody:   healthyResponseBody,
		},
		"not ready": {
			check: func(context.Context) error {
				return errReadinessUnavailable
			},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   unhealthyResponseBody,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			handler := newHTTPHandler(http.NotFoundHandler(), test.check, time.Second)
			response := serveRequest(handler, http.MethodGet, readinessEndpointPath)

			assertHealthResponse(t, response, test.wantStatus, test.wantBody)
		})
	}
}

func TestReadinessEndpointBoundsCheckDuration(t *testing.T) {
	t.Parallel()

	const timeout = 2 * time.Second

	handler := newHTTPHandler(
		http.NotFoundHandler(),
		func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("readiness check context has no deadline")
				return nil
			}

			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > timeout {
				t.Errorf("readiness deadline remaining = %v, want within (0, %v]", remaining, timeout)
			}
			return nil
		},
		timeout,
	)

	response := serveRequest(handler, http.MethodGet, readinessEndpointPath)

	assertHealthResponse(t, response, http.StatusOK, healthyResponseBody)
}

func TestHealthEndpointsSupportHead(t *testing.T) {
	t.Parallel()

	handler := newHTTPHandler(
		http.NotFoundHandler(),
		func(context.Context) error {
			return nil
		},
		time.Second,
	)

	for _, path := range []string{livenessEndpointPath, readinessEndpointPath} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			response := serveRequest(handler, http.MethodHead, path)

			assertHealthResponse(t, response, http.StatusOK, "")
		})
	}
}

func TestHealthEndpointsRejectUnsupportedMethods(t *testing.T) {
	t.Parallel()

	handler := newHTTPHandler(
		http.NotFoundHandler(),
		func(context.Context) error {
			return nil
		},
		time.Second,
	)

	for _, path := range []string{livenessEndpointPath, readinessEndpointPath} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			response := serveRequest(handler, http.MethodPost, path)

			if response.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
			}
			if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("Allow = %q, want GET, HEAD", allow)
			}
		})
	}
}

func TestHTTPHandlerRoutesOtherPathsToProxy(t *testing.T) {
	t.Parallel()

	proxyCalled := false
	proxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := newHTTPHandler(
		proxy,
		func(context.Context) error {
			return nil
		},
		time.Second,
	)

	response := serveRequest(handler, http.MethodGet, "/nar/example.nar")

	if response.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if !proxyCalled {
		t.Error("proxy was not called")
	}
}

func TestReadinessCheckCachesResults(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC)
	var calls int
	cache := &readinessResultCache{
		check: func(context.Context) error {
			calls++
			return errReadinessUnavailable
		},
		ttl: time.Second,
		now: func() time.Time {
			return now
		},
	}

	if err := cache.run(context.Background()); !errors.Is(err, errReadinessUnavailable) {
		t.Fatalf("first check error = %v, want %v", err, errReadinessUnavailable)
	}
	if err := cache.run(context.Background()); !errors.Is(err, errReadinessUnavailable) {
		t.Fatalf("cached check error = %v, want %v", err, errReadinessUnavailable)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 before expiry", calls)
	}

	now = now.Add(time.Second)
	if err := cache.run(context.Background()); !errors.Is(err, errReadinessUnavailable) {
		t.Fatalf("refreshed check error = %v, want %v", err, errReadinessUnavailable)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 after expiry", calls)
	}
}

func TestReadinessCheckCoalescesConcurrentCalls(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	check := cacheReadinessCheck(
		func(context.Context) error {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return nil
		},
		time.Second,
	)

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- check(context.Background())
	}()
	<-started

	secondResult := make(chan error, 1)
	go func() {
		secondResult <- check(context.Background())
	}()

	close(release)
	if err := <-firstResult; err != nil {
		t.Errorf("first check error = %v, want nil", err)
	}
	if err := <-secondResult; err != nil {
		t.Errorf("second check error = %v, want nil", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

func TestReadinessCheckWaitHonorsContext(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	check := cacheReadinessCheck(
		func(context.Context) error {
			close(started)
			<-release
			return nil
		},
		time.Second,
	)

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- check(context.Background())
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := check(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waiting check error = %v, want context canceled", err)
	}

	close(release)
	if err := <-firstResult; err != nil {
		t.Errorf("first check error = %v, want nil", err)
	}
}

func serveRequest(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	return response
}

func assertHealthResponse(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantStatus int,
	wantBody string,
) {
	t.Helper()

	if response.Code != wantStatus {
		t.Errorf("status = %d, want %d", response.Code, wantStatus)
	}
	if response.Body.String() != wantBody {
		t.Errorf("body = %q, want %q", response.Body.String(), wantBody)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != healthResponseCacheMode {
		t.Errorf("Cache-Control = %q, want %q", cacheControl, healthResponseCacheMode)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", contentType)
	}
}
