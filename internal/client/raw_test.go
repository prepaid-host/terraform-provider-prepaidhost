// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

func rawServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// GET /rootserver/{id}/rental answers a bare object without envelope.
func TestDoRaw_BareObject(t *testing.T) {
	url := rawServer(t, 200, `{"created_rental":"2026-06-10T16:44:35.000000Z","delete_rental":"2026-08-09T16:44:35.000000Z"}`)
	var out struct {
		Created string `json:"created_rental"`
		Delete  string `json:"delete_rental"`
	}
	if err := client.New(url, "t").DoRaw(context.Background(), http.MethodGet, "/rootserver/1/rental", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Delete != "2026-08-09T16:44:35.000000Z" || out.Created == "" {
		t.Errorf("got %+v", out)
	}
}

// GET /rootserver/{id}/addresses answers a bare array.
func TestDoRaw_BareArray(t *testing.T) {
	url := rawServer(t, 200, `[{"id":1,"address":"203.0.113.5"}]`)
	var out []struct {
		Address string `json:"address"`
	}
	if err := client.New(url, "t").DoRaw(context.Background(), http.MethodGet, "/rootserver/1/addresses", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Address != "203.0.113.5" {
		t.Errorf("got %+v", out)
	}
}

func TestDoRaw_ErrorsKeepTheirEnvelope(t *testing.T) {
	cases := map[string]struct {
		status   int
		body     string
		notFound bool
	}{
		"legacy 404":       {404, `{"success":false,"result":"Server not found."}`, true},
		"abort 404":        {404, `{"success":false,"error":{"code":"NOT_FOUND","message":"Endpoint nicht gefunden."}}`, true},
		"legacy 200":       {200, `{"success":false,"result":"No Entrys available."}`, true},
		"html 502":         {502, `<html>bad gateway</html>`, false},
		"validation error": {422, `{"success":false,"error":{"code":"VALIDATION_ERROR","message":"x","details":{"a":["b"]}}}`, false},
	}
	for name, tc := range cases {
		url := rawServer(t, tc.status, tc.body)
		var out map[string]any
		err := client.New(url, "t", client.WithMaxRetries(0)).DoRaw(context.Background(), http.MethodGet, "/x", nil, &out)
		if _, ok := client.AsAPIError(err); !ok {
			t.Errorf("%s: want APIError, got %v", name, err)
			continue
		}
		if got := client.IsNotFound(err); got != tc.notFound {
			t.Errorf("%s: IsNotFound = %v, want %v", name, got, tc.notFound)
		}
	}
}

func TestDoRaw_GETIsRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	var out []any
	if err := client.New(srv.URL, "t", fastRetries).DoRaw(context.Background(), http.MethodGet, "/x", nil, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}
