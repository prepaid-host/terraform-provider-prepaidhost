// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

// throttlingServer answers the first `refusals` requests with 429 (with the
// given Retry-After, "" = none), then succeeds.
func throttlingServer(t *testing.T, refusals int32, retryAfter string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= refusals {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"success":false,"error":{"code":"TOO_MANY_REQUESTS","message":"Zu viele Anfragen."}}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestThrottle_GETWaitsForRetryAfter(t *testing.T) {
	srv, calls := throttlingServer(t, 1, "1")
	start := time.Now()
	if err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodGet, "/account/ssh-keys", nil, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	// fastRetries would retry after 1ms; Retry-After asks for a second.
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Errorf("retried after %s, want Retry-After (1s) honoured", waited)
	}
}

// 429 is retried for what is safe to repeat: GET, the quote (prices only) and
// the order with an Idempotency-Key. Other writes are never repeated.
func TestThrottle_RetriedOnlyWhereSafe(t *testing.T) {
	key := map[string]string{"Idempotency-Key": "tf-0123456789"}
	cases := []struct {
		method, path string
		headers      map[string]string
		retried      bool
	}{
		{http.MethodGet, "/orders/abc", nil, true},
		{http.MethodPost, "/orders/quote", nil, true},
		{http.MethodPost, "/orders", key, true},
		{http.MethodPost, "/orders", nil, false},
		{http.MethodPost, "/account/ssh-keys", key, false},
		{http.MethodPut, "/domain/1/nameservers", nil, false},
		{http.MethodDelete, "/account/ssh-keys/1", nil, false},
	}
	for _, tc := range cases {
		srv, calls := throttlingServer(t, 1, "")
		err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), tc.method, tc.path, map[string]any{}, nil, tc.headers)
		want := int32(1)
		if tc.retried {
			want = 2
		}
		if calls.Load() != want {
			t.Errorf("%s %s: calls = %d, want %d", tc.method, tc.path, calls.Load(), want)
		}
		if tc.retried && err != nil {
			t.Errorf("%s %s: %v", tc.method, tc.path, err)
		}
		if !tc.retried && !client.IsThrottled(err) {
			t.Errorf("%s %s: err = %v, want the 429", tc.method, tc.path, err)
		}
	}
}

// 5xx retries of the quote stay off: only the throttle answer is known not to
// have been processed.
func TestThrottle_QuoteIsNotRetriedOn5xx(t *testing.T) {
	srv, calls := countingServer(t, 1)
	if err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodPost, "/orders/quote", map[string]any{}, nil, nil); err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestThrottle_GivesUpAfterMaxThrottleRetries(t *testing.T) {
	srv, calls := throttlingServer(t, 100, "")
	err := client.New(srv.URL, "t", fastRetries, client.WithMaxThrottleRetries(2)).Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	if !client.IsThrottled(err) || !client.IsCode(err, client.CodeTooManyRequests) {
		t.Fatalf("err = %v, want the 429", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (1 + 2 retries)", calls.Load())
	}
}

// The default throttle budget is larger than the 5xx budget (3).
func TestThrottle_DefaultBudgetOutlastsThe5xxBudget(t *testing.T) {
	srv, calls := throttlingServer(t, 6, "")
	if err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodGet, "/x", nil, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 7 {
		t.Errorf("calls = %d, want 7", calls.Load())
	}
}

// A Retry-After beyond the context's deadline is not waited for: the 429
// comes back at once instead of a context error after a pointless wait.
func TestThrottle_RetryAfterBeyondDeadlineReturnsAtOnce(t *testing.T) {
	srv, calls := throttlingServer(t, 100, "60")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := client.New(srv.URL, "t", fastRetries).Do(ctx, http.MethodGet, "/x", nil, nil, nil)
	if !client.IsThrottled(err) {
		t.Fatalf("err = %v, want the 429", err)
	}
	if time.Since(start) > time.Second || calls.Load() != 1 {
		t.Errorf("waited %s with %d calls, want an immediate return", time.Since(start), calls.Load())
	}
}

func TestThrottle_WaitEndsWithContext(t *testing.T) {
	srv, _ := throttlingServer(t, 100, "30")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := client.New(srv.URL, "t", fastRetries).Do(ctx, http.MethodGet, "/x", nil, nil, nil)
	if !errors.Is(err, context.Canceled) || !client.IsThrottled(err) {
		t.Fatalf("err = %v, want the 429 joined with the cancellation", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("wait ignored the cancellation")
	}
}

func TestThrottle_MaxRetryAfterCapsTheWait(t *testing.T) {
	srv, calls := throttlingServer(t, 1, "30")
	start := time.Now()
	err := client.New(srv.URL, "t", fastRetries, client.WithMaxRetryAfter(10*time.Millisecond)).
		Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Retry-After was not capped")
	}
}

// A refresh of many resources in parallel runs into the general limit of the
// mock API and recovers by waiting for Retry-After.
func TestThrottle_ParallelReadsUnderTheLimitRecover(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(10)
	api.Throttle(10, time.Second)
	c := client.New(api.URL, api.Token, fastRetries)

	const readers = 30
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]any
			errs <- c.Do(context.Background(), http.MethodGet, "/account/balance", nil, &out, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("read failed: %v", err)
		}
	}
	if api.ThrottledCount() == 0 {
		t.Error("the limit never refused a request; the test proves nothing")
	}
}

func TestCheckEndpoint(t *testing.T) {
	api := testapi.New(t)
	if err := client.New(api.URL, api.Token).CheckEndpoint(context.Background()); err != nil {
		t.Errorf("mock API: %v", err)
	}
	// A rejected token does not make the endpoint wrong.
	if err := client.New(api.URL, "wrong").CheckEndpoint(context.Background()); err != nil {
		t.Errorf("wrong token: %v", err)
	}
	// A path without a route: ENDPOINT_NOT_FOUND.
	if err := client.New(api.URL+"/v2", api.Token).CheckEndpoint(context.Background()); !errors.Is(err, client.ErrNotTheAPI) {
		t.Errorf("unknown route: err = %v", err)
	}

	cases := map[string]struct {
		status int
		body   string
		wrong  bool
	}{
		"HTML page":            {200, "<html><body>Welcome</body></html>", true},
		"HTML 404":             {404, "<html>404</html>", true},
		"enveloped 404":        {404, `{"success":false,"error":{"code":"NOT_FOUND","message":"Endpoint nicht gefunden."}}`, true},
		"server error":         {500, `{"success":false,"error":{"code":"INTERNAL_ERROR","message":"x"}}`, false},
		"rate limit":           {429, `{"success":false,"error":{"code":"TOO_MANY_REQUESTS","message":"x"}}`, false},
		"unauthenticated JSON": {401, `{"success":false,"error":{"code":"UNAUTHENTICATED","message":"x"}}`, false},
	}
	for name, tc := range cases {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/api/catalog" {
				t.Errorf("%s: probed %s", name, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		err := client.New(srv.URL+"/api", "t", fastRetries).CheckEndpoint(context.Background())
		srv.Close()
		if got := errors.Is(err, client.ErrNotTheAPI); got != tc.wrong {
			t.Errorf("%s: err = %v, want wrong = %v", name, err, tc.wrong)
		}
		if calls.Load() != 1 {
			t.Errorf("%s: calls = %d, want exactly one (no retries)", name, calls.Load())
		}
	}

	// Unreachable: left to the real calls.
	if err := client.New("http://127.0.0.1:9/api", "t").CheckEndpoint(context.Background()); err != nil {
		t.Errorf("unreachable: %v", err)
	}
}

func TestIsNotFound_EndpointNotFoundIsNotAResource(t *testing.T) {
	err := &client.APIError{Status: 404, Code: client.CodeEndpointNotFound, Message: "Endpoint nicht gefunden.", Enveloped: true}
	if client.IsNotFound(err) {
		t.Error("ENDPOINT_NOT_FOUND must not count as a missing resource")
	}

	api := testapi.New(t)
	derr := client.New(api.URL, api.Token).Do(context.Background(), http.MethodGet, "/no-such-route/1", nil, nil, nil)
	if !client.IsCode(derr, client.CodeEndpointNotFound) || client.IsNotFound(derr) {
		t.Errorf("mock unknown route: err = %v, IsNotFound = %v", derr, client.IsNotFound(derr))
	}
	if !strings.Contains(derr.Error(), "404") {
		t.Errorf("err = %v, want HTTP 404", derr)
	}
}
