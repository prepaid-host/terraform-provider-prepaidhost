// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		" 3 ":                           3 * time.Second,
		"-1":                            0,
		"soon":                          0,
		"Mon, 26 Oct 2026 12:00:05 GMT": 5 * time.Second,
		"Mon, 26 Oct 2026 11:59:00 GMT": 0,
	}
	for v, want := range cases {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		if got := retryAfter(h, now); got != want {
			t.Errorf("Retry-After %q: %s, want %s", v, got, want)
		}
	}
}
