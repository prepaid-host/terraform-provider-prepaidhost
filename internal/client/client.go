// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

// Package client is a small client for the Prepaid-Host REST API
// (https://prepaid-host.com/api). Every response uses the envelope
// {"success":true,"result":...} or {"success":false,"error":{...}}.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout      = 60 * time.Second
	defaultMaxRetries   = 3
	defaultRetryBackoff = time.Second
	defaultPollInterval = 10 * time.Second
	maxBackoff          = 30 * time.Second
	maxErrorBodyLength  = 512
	maxRedirects        = 10
)

// Client talks to the Prepaid-Host API with a Bearer token.
type Client struct {
	endpoint     string
	token        string
	httpClient   *http.Client
	userAgent    string
	maxRetries   int
	retryBackoff time.Duration
	pollInterval time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			cp := *hc
			if cp.CheckRedirect == nil {
				cp.CheckRedirect = checkRedirect
			}
			c.httpClient = &cp
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithMaxRetries sets how often a retryable request is repeated (default 3).
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithRetryBackoff sets the wait before the first retry; it doubles per retry
// (default 1s).
func WithRetryBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.retryBackoff = d
		}
	}
}

// WithPollInterval sets the interval returned by PollInterval (default 10s),
// used when waiting for an order to be provisioned.
func WithPollInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.pollInterval = d
		}
	}
}

// New returns a client for the API at endpoint (e.g. https://prepaid-host.com/api).
func New(endpoint, token string, opts ...Option) *Client {
	c := &Client{
		endpoint:     strings.TrimRight(endpoint, "/"),
		token:        token,
		httpClient:   &http.Client{Timeout: defaultTimeout, CheckRedirect: checkRedirect},
		userAgent:    "terraform-provider-prepaidhost",
		maxRetries:   defaultMaxRetries,
		retryBackoff: defaultRetryBackoff,
		pollInterval: defaultPollInterval,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// checkRedirect follows redirects only for GET. For writes Go would turn a
// 301/302/303 into a GET and silently drop the body; instead the 3xx answer
// is returned and reported as an error.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && via[0].Method != http.MethodGet {
		return http.ErrUseLastResponse
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// Endpoint returns the API base URL without trailing slash.
func (c *Client) Endpoint() string { return c.endpoint }

// Token returns the Bearer token.
func (c *Client) Token() string { return c.token }

// PollInterval is the interval for polling an order's provisioning status.
func (c *Client) PollInterval() time.Duration { return c.pollInterval }

// Do sends a request and decodes the envelope's result into out (if out is
// not nil). body is sent as JSON when not nil. A failed call returns an
// *APIError; transport failures are returned as they are.
//
// Retries (network error or HTTP 5xx, up to WithMaxRetries times, with
// exponential backoff) happen only for GET and for POST /orders carrying an
// Idempotency-Key header — never for other writes, which could otherwise run
// twice.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any, headers map[string]string) error {
	_, err := c.do(ctx, method, path, body, out, headers)
	return err
}

// DoRaw is Do for the older endpoints that answer success without the
// envelope — a bare object or array, e.g. GET /rootserver/{id}/rental and
// GET /rootserver/{id}/addresses. The whole body is decoded into out. Error
// answers (HTTP >= 400, or any {"success":false,...}) are decoded like Do's,
// so client.IsNotFound works the same. Retries follow Do's rules.
func (c *Client) DoRaw(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.send(ctx, method, path, body, out, nil, decodeRaw)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, headers map[string]string) (http.Header, error) {
	return c.send(ctx, method, path, body, out, headers, decode)
}

type decoder func(status int, raw []byte, out any) error

func (c *Client) send(ctx context.Context, method, path string, body any, out any, headers map[string]string, dec decoder) (http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
	}

	url := c.endpoint + "/" + strings.TrimLeft(path, "/")
	retryable := isRetryable(method, path, headers)

	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, errors.Join(lastErr, err)
			}
		}

		header, err := c.once(ctx, method, url, payload, out, headers, dec)
		if err == nil {
			return header, nil
		}
		lastErr = err

		if !retryable || attempt >= c.maxRetries || !isTransient(err) || ctx.Err() != nil {
			return header, err
		}
	}
}

func (c *Client) once(ctx context.Context, method, url string, payload []byte, out any, headers map[string]string, dec decoder) (http.Header, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &transportError{err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.Header, &transportError{err: fmt.Errorf("reading response: %w", err)}
	}

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.Header, fmt.Errorf("%s %s was redirected (HTTP %d to %q); redirects are not followed for writes — check the provider endpoint",
			method, url, resp.StatusCode, resp.Header.Get("Location"))
	}

	return resp.Header, dec(resp.StatusCode, raw, out)
}

type envelope struct {
	Success *bool           `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func decode(status int, raw []byte, out any) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Success == nil {
		if status >= 400 {
			return &APIError{Status: status, Message: snippet(raw)}
		}
		return fmt.Errorf("unexpected response (HTTP %d): %s", status, snippet(raw))
	}

	if status >= 400 || !*env.Success {
		return envelopeError(status, env)
	}

	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decoding result: %w", err)
	}
	return nil
}

// decodeRaw decodes a body without envelope; see DoRaw.
func decodeRaw(status int, raw []byte, out any) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Success != nil && (status >= 400 || !*env.Success) {
		return envelopeError(status, env)
	}
	if status >= 400 {
		return &APIError{Status: status, Message: snippet(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response (HTTP %d): %w: %s", status, err, snippet(raw))
	}
	return nil
}

func envelopeError(status int, env envelope) *APIError {
	apiErr := &APIError{Status: status, Enveloped: true}

	var fields map[string]any
	if len(env.Error) > 0 && json.Unmarshal(env.Error, &fields) == nil && fields != nil {
		apiErr.Code, _ = fields["code"].(string)
		apiErr.Message, _ = fields["message"].(string)
		delete(fields, "code")
		delete(fields, "message")
		if len(fields) > 0 {
			apiErr.Extra = fields
		}
		return apiErr
	}

	// Older endpoints: {"success":false,"result":"message"} or a validation object.
	var msg string
	if json.Unmarshal(env.Result, &msg) == nil {
		apiErr.Message = msg
	} else if len(env.Result) > 0 {
		apiErr.Message = string(env.Result)
	}
	return apiErr
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > maxErrorBodyLength {
		s = s[:maxErrorBodyLength] + "…"
	}
	return s
}

// transportError marks a failure before an HTTP response was read.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func isTransient(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	apiErr, ok := AsAPIError(err)
	return ok && apiErr.Status >= 500
}

// isRetryable implements the retry rule: GET always, POST /orders only with an
// Idempotency-Key (the server then replays instead of charging twice).
func isRetryable(method, path string, headers map[string]string) bool {
	switch method {
	case http.MethodGet:
		return true
	case http.MethodPost:
		p := path
		if i := strings.IndexAny(p, "?#"); i >= 0 {
			p = p[:i]
		}
		p = "/" + strings.Trim(p, "/")
		return p == "/orders" && headerValue(headers, "Idempotency-Key") != ""
	}
	return false
}

func headerValue(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.retryBackoff
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
