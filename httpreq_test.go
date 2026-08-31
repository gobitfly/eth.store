package ethstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testClient returns a client paced fast enough that the shared ratelimiter
// does not dominate the runtime of these tests.
func testClient() *BeaconchainApiClient {
	c := NewBeaconchainApiClient()
	c.ratelimiter.SetRate(1000)
	return c
}

func TestHttpReqRetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("ratelimit-reset", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	res := &BeaconchainDepositRequestsResponse{}
	if err := testClient().HttpReq(context.Background(), http.MethodGet, srv.URL, nil, nil, res); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts (2 ratelimited + 1 success), got %d", got)
	}
}

func TestHttpReqGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("ratelimit-reset", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := testClient().HttpReq(context.Background(), http.MethodGet, srv.URL, nil, nil, nil)

	var reqErr HttpReqError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected an HttpReqError, got %T: %v", err, err)
	}
	if reqErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the final error to keep status 429, got %d", reqErr.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != httpReqMaxAttempts {
		t.Fatalf("expected exactly %d attempts, got %d", httpReqMaxAttempts, got)
	}
}

func TestHttpReqDoesNotRetryOtherStatuses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := testClient().HttpReq(context.Background(), http.MethodGet, srv.URL, nil, nil, nil); err == nil {
		t.Fatal("expected a 500 to be returned as an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a non-429 must not be retried, got %d attempts", got)
	}
}

// A transport error leaves http.Client.Do returning a nil response, which the
// error path used to dereference.
func TestHttpReqTransportErrorDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	err := testClient().HttpReq(context.Background(), http.MethodGet, url, nil, nil, nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}

	var reqErr HttpReqError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected an HttpReqError, got %T: %v", err, err)
	}
	if reqErr.HTTPError == nil {
		t.Fatal("expected the underlying transport error to be preserved")
	}
	if reqErr.StatusCode != 0 {
		t.Fatalf("expected no status code for a transport error, got %d", reqErr.StatusCode)
	}
}

func TestHttpReqDoesNotRetryWhenResetIsTooFarAway(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("ratelimit-reset", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	start := time.Now()
	err := testClient().HttpReq(context.Background(), http.MethodGet, srv.URL, nil, nil, nil)

	var reqErr HttpReqError
	if !errors.As(err, &reqErr) || reqErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the 429 to be returned, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a reset beyond httpReqMaxRetryDelay must fail fast, got %d attempts", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("expected an immediate failure, took %v", elapsed)
	}
}

func TestSetRatelimitTargetFraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ratelimit-limit", "100")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := testClient()
	if err := c.HttpReq(context.Background(), http.MethodGet, srv.URL, nil, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.ratelimiter.GetRate(); got != 100*defaultRatelimitTargetFraction {
		t.Fatalf("expected the default fraction to pace to %v, got %v", 100*defaultRatelimitTargetFraction, got)
	}

	c.SetRatelimitTargetFraction(.2)
	if err := c.HttpReq(context.Background(), http.MethodGet, srv.URL, nil, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.ratelimiter.GetRate(); got != 20 {
		t.Fatalf("expected the configured fraction to pace to 20, got %v", got)
	}

	c.SetRatelimitTargetFraction(0)
	c.SetRatelimitTargetFraction(1.5)
	if got := c.GetRatelimitTargetFraction(); got != .2 {
		t.Fatalf("values outside (0, 1] must be ignored, got %v", got)
	}
}

func TestHttpReqStopsRetryingWhenContextIsCancelled(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("ratelimit-reset", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := testClient().HttpReq(ctx, http.MethodGet, srv.URL, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the cancelled context to be returned, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("expected the wait to be cut short by the context, took %v", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected to stop after the first ratelimited attempt, got %d", got)
	}
}
