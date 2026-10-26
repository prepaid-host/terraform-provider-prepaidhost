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
	"sync/atomic"
	"testing"
	"time"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

// fastRetries keeps the retry tests quick.
var fastRetries = client.WithRetryBackoff(time.Millisecond)

func TestDo_SendsBearerTokenAndJSONHeaders(t *testing.T) {
	var got http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"success":true,"result":{"balance":12.5,"currency":"EUR"}}`)
	}))
	defer srv.Close()

	c := client.New(srv.URL+"/api/", "secret-token", client.WithUserAgent("terraform-provider-prepaidhost/test"))

	var out struct {
		Balance  float64 `json:"balance"`
		Currency string  `json:"currency"`
	}
	if err := c.Do(context.Background(), http.MethodGet, "/account/balance", nil, &out, map[string]string{"X-Extra": "1"}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if gotPath != "/api/account/balance" {
		t.Errorf("path = %q, want /api/account/balance", gotPath)
	}
	if v := got.Get("Authorization"); v != "Bearer secret-token" {
		t.Errorf("Authorization = %q", v)
	}
	if v := got.Get("Accept"); v != "application/json" {
		t.Errorf("Accept = %q", v)
	}
	if v := got.Get("User-Agent"); v != "terraform-provider-prepaidhost/test" {
		t.Errorf("User-Agent = %q", v)
	}
	if v := got.Get("X-Extra"); v != "1" {
		t.Errorf("X-Extra = %q", v)
	}
	if out.Balance != 12.5 || out.Currency != "EUR" {
		t.Errorf("result not decoded: %+v", out)
	}
}

func TestDo_SendsJSONBody(t *testing.T) {
	var body, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"success":true,"result":{"id":7}}`)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "t")
	if err := c.Do(context.Background(), http.MethodPost, "account/ssh-keys", map[string]string{"name": "laptop"}, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if body != `{"name":"laptop"}` {
		t.Errorf("body = %q", body)
	}
}

func TestDo_ErrorEnvelopeBecomesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"success":false,"error":{"code":"INSUFFICIENT_BALANCE","message":"Guthaben reicht nicht.","missing_amount":4.5}}`)
	}))
	defer srv.Close()

	err := client.New(srv.URL, "t").Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)

	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *client.APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusPaymentRequired {
		t.Errorf("Status = %d", apiErr.Status)
	}
	if apiErr.Code != "INSUFFICIENT_BALANCE" {
		t.Errorf("Code = %q", apiErr.Code)
	}
	if apiErr.Message != "Guthaben reicht nicht." {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if v, ok := apiErr.Float("missing_amount"); !ok || v != 4.5 {
		t.Errorf("missing_amount = %v, %v (Extra=%v)", v, ok, apiErr.Extra)
	}
	if _, ok := apiErr.Extra["code"]; ok {
		t.Errorf("Extra must not repeat code/message: %v", apiErr.Extra)
	}
	if !strings.Contains(apiErr.Error(), "INSUFFICIENT_BALANCE") || !strings.Contains(apiErr.Error(), "Guthaben reicht nicht.") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
	if !client.IsCode(err, client.CodeInsufficientBalance) {
		t.Errorf("IsCode false")
	}
}

func TestDo_ConflictExtraCarriesResourceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"success":false,"error":{"code":"CLIENT_REFERENCE_IN_USE","message":"in use","resource_type":"rootserver","resource_id":4711}}`)
	}))
	defer srv.Close()

	err := client.New(srv.URL, "t").Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		t.Fatalf("not an APIError: %v", err)
	}
	if id, ok := apiErr.Int64("resource_id"); !ok || id != 4711 {
		t.Errorf("resource_id = %d, %v", id, ok)
	}
	if typ, ok := apiErr.String("resource_type"); !ok || typ != "rootserver" {
		t.Errorf("resource_type = %q, %v", typ, ok)
	}
}

// Legacy endpoints answer "not found" with HTTP 200 and {"success":false,"result":"..."}.
func TestDo_LegacyFailureWithStatus200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":false,"result":"No Entrys available."}`)
	}))
	defer srv.Close()

	err := client.New(srv.URL, "t").Do(context.Background(), http.MethodGet, "/rootserver/1", nil, nil, nil)
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.Status != http.StatusOK || apiErr.Code != "" || apiErr.Message != "No Entrys available." {
		t.Errorf("got %+v", apiErr)
	}
}

func TestDo_NonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	defer srv.Close()

	err := client.New(srv.URL, "t", client.WithMaxRetries(0)).Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.Status != http.StatusBadGateway || !strings.Contains(apiErr.Message, "bad gateway") {
		t.Errorf("got %+v", apiErr)
	}
}

func TestIsNotFound(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"enveloped 404":         {&client.APIError{Status: 404, Code: "ORDER_NOT_FOUND", Enveloped: true}, true},
		"enveloped legacy 404":  {&client.APIError{Status: 404, Message: "Server not found.", Enveloped: true}, true},
		"legacy 200":            {&client.APIError{Status: 200, Message: "No Entrys available.", Enveloped: true}, true},
		"legacy domain 200":     {&client.APIError{Status: 200, Message: "Domain not found.", Enveloped: true}, true},
		"404 without envelope":  {&client.APIError{Status: 404, Message: "<html>404</html>"}, false},
		"422 template":          {&client.APIError{Status: 422, Message: "Template not found.", Enveloped: true}, false},
		"422 NOT_FOUND code":    {&client.APIError{Status: 422, Code: "DOMAIN_NOT_FOUND", Enveloped: true}, false},
		"200 with code":         {&client.APIError{Status: 200, Code: "X", Message: "not found", Enveloped: true}, false},
		"other":                 {&client.APIError{Status: 409, Code: "PRICE_CHANGED", Enveloped: true}, false},
		"plain error":           {errors.New("boom"), false},
		"nil":                   {nil, false},
		"legacy 200 other text": {&client.APIError{Status: 200, Message: "Etwas anderes ging schief.", Enveloped: true}, false},
	}
	for name, tc := range cases {
		if got := client.IsNotFound(tc.err); got != tc.want {
			t.Errorf("%s: IsNotFound = %v, want %v", name, got, tc.want)
		}
	}
}

func TestIsNotFound_FromResponses(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   bool
	}{
		"HTML 404 from proxy": {404, "<html><body>404 Not Found</body></html>", false},
		"enveloped 404":       {404, `{"success":false,"error":{"code":"ORDER_NOT_FOUND","message":"nope"}}`, true},
		"legacy 200":          {200, `{"success":false,"result":"No Entrys available."}`, true},
		"422 template":        {422, `{"success":false,"result":"Template not found."}`, false},
	}
	for name, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		err := client.New(srv.URL, "t").Do(context.Background(), http.MethodGet, "/rootserver/1", nil, nil, nil)
		srv.Close()
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		if got := client.IsNotFound(err); got != tc.want {
			t.Errorf("%s: IsNotFound = %v, want %v (err %v)", name, got, tc.want, err)
		}
	}
}

func TestRedirect_WritesAreNotFollowed(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		var targetHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/target" {
				targetHits.Add(1)
				_, _ = io.WriteString(w, `{"success":true,"result":{}}`)
				return
			}
			http.Redirect(w, r, "/target", http.StatusFound)
		}))
		err := client.New(srv.URL, "t").Do(context.Background(), method, "/rootserver/1/autorenew", map[string]any{"enabled": false}, nil, nil)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Errorf("%s: err = %v, want redirect error", method, err)
		}
		if targetHits.Load() != 0 {
			t.Errorf("%s: redirect was followed", method)
		}
	}
}

func TestRedirect_GETIsFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			_, _ = io.WriteString(w, `{"success":true,"result":{"ok":true}}`)
			return
		}
		http.Redirect(w, r, "/target", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	var out struct{ OK bool }
	if err := client.New(srv.URL, "t").Do(context.Background(), http.MethodGet, "/x", nil, &out, nil); err != nil || !out.OK {
		t.Errorf("err = %v, out = %+v", err, out)
	}
}

func TestRedirect_CustomHTTPClientGetsPolicy(t *testing.T) {
	var targetHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetHits.Add(1)
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer srv.Close()

	hc := &http.Client{}
	_ = client.New(srv.URL, "t", client.WithHTTPClient(hc)).Do(context.Background(), http.MethodPut, "/x", map[string]any{}, nil, nil)
	if targetHits.Load() != 0 {
		t.Errorf("redirect was followed with custom client")
	}
	if hc.CheckRedirect != nil {
		t.Errorf("caller's client must not be modified")
	}
}

// countingServer answers the first `failures` requests with 503, then succeeds.
func countingServer(t *testing.T, failures int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"success":false,"error":{"code":"UNAVAILABLE","message":"try later"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true,"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestRetry_GETIsRetriedOn5xx(t *testing.T) {
	srv, calls := countingServer(t, 2)
	err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodGet, "/orders/abc", nil, nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestRetry_GETGivesUpAfterThreeRetries(t *testing.T) {
	srv, calls := countingServer(t, 100)
	err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 4 {
		t.Errorf("calls = %d, want 4 (1 + 3 retries)", calls.Load())
	}
}

func TestRetry_GETIsNotRetriedOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"success":false,"error":{"code":"ORDER_NOT_FOUND","message":"nope"}}`)
	}))
	defer srv.Close()

	_ = client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRetry_GETIsRetriedOnNetworkError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no hijacker")
				return
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"result":{}}`)
	}))
	defer srv.Close()

	if err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodGet, "/x", nil, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestRetry_PostOrdersWithIdempotencyKeyIsRetried(t *testing.T) {
	srv, calls := countingServer(t, 1)
	err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodPost, "/orders", map[string]any{"items": []any{}}, nil,
		map[string]string{"Idempotency-Key": "tf-0123456789"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestRetry_PostOrdersWithoutKeyIsNotRetried(t *testing.T) {
	srv, calls := countingServer(t, 1)
	err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodPost, "/orders", map[string]any{}, nil, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRetry_OtherPostIsNeverRetried(t *testing.T) {
	for _, path := range []string{"/orders/quote", "/account/ssh-keys", "/rootserver/1/renew"} {
		srv, calls := countingServer(t, 1)
		err := client.New(srv.URL, "t", fastRetries).Do(context.Background(), http.MethodPost, path, map[string]any{}, nil,
			map[string]string{"Idempotency-Key": "tf-0123456789"})
		if err == nil {
			t.Errorf("%s: want error", path)
		}
		if calls.Load() != 1 {
			t.Errorf("%s: calls = %d, want 1", path, calls.Load())
		}
	}
}

func TestRetry_PutAndDeleteAreNeverRetried(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		srv, calls := countingServer(t, 1)
		_ = client.New(srv.URL, "t", fastRetries).Do(context.Background(), method, "/rootserver/1/autorenew", map[string]any{"enabled": false}, nil, nil)
		if calls.Load() != 1 {
			t.Errorf("%s: calls = %d, want 1", method, calls.Load())
		}
	}
}

func TestRetry_StopsWhenContextIsCancelled(t *testing.T) {
	srv, _ := countingServer(t, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.New(srv.URL, "t", client.WithRetryBackoff(time.Hour)).Do(ctx, http.MethodGet, "/x", nil, nil, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("retry ignored context cancellation")
	}
}
