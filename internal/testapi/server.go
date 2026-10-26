// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

// Package testapi is an in-process mock of the Prepaid-Host API for tests.
// No test of this provider talks to the real API: they all start a Server and
// point the provider (or client) at Server.URL.
//
// The mock keeps its state (balance, resources, orders, idempotency keys) in
// memory behind one mutex. Endpoints live in a route table; each file of this
// package registers its endpoints in an init function via register, so a new
// endpoint is a new handler plus one Handle call. Tests can additionally
// override routes (Handle), queue canned responses or dropped connections
// for a route (InjectResponse), switch on the API's rate limits (Throttle,
// ThrottleRoutes) and inspect the requests made (CallCount, Requests).
//
// Like the real API, a path without a route answers 404 ENDPOINT_NOT_FOUND
// (a path that exists for another method: 405 METHOD_NOT_ALLOWED); a
// resource that does not exist answers 404 NOT_FOUND.
package testapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultToken is the Bearer token the mock accepts unless Server.Token is changed.
const DefaultToken = "test-token"

// HandlerFunc answers one request. It runs with the server's mutex held, so
// it may read and change State freely but must not call methods of Server.
type HandlerFunc func(s *State, r *Request) Response

// Request is an incoming request with its path parameters and raw body.
type Request struct {
	*http.Request

	// Path is the request path without the /api prefix, e.g. /orders/abc.
	Path   string
	Params map[string]string
	Body   []byte
}

// Param returns a path parameter, e.g. "id" for /rootserver/{id}.
func (r *Request) Param(name string) string { return r.Params[name] }

// ParamInt64 returns a numeric path parameter.
func (r *Request) ParamInt64(name string) (int64, bool) {
	v, err := strconv.ParseInt(r.Params[name], 10, 64)
	return v, err == nil
}

// Decode unmarshals the JSON body into v.
func (r *Request) Decode(v any) error {
	if len(bytes.TrimSpace(r.Body)) == 0 {
		return fmt.Errorf("empty body")
	}
	return json.Unmarshal(r.Body, v)
}

// BodyMap returns the JSON body as a map (empty map if there is none or it is invalid).
func (r *Request) BodyMap() map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// Response is what a handler (or an injected response) sends back.
type Response struct {
	Status int
	// Body is marshalled to JSON as is. Use OK/Created/Fail/Legacy to build envelopes.
	Body   any
	Header http.Header

	drop         bool
	afterHandler bool
}

type successEnvelope struct {
	Success bool `json:"success"`
	Result  any  `json:"result"`
}

// OK is a 200 success envelope.
func OK(result any) Response {
	return Response{Status: http.StatusOK, Body: successEnvelope{Success: true, Result: result}}
}

// Created is a 201 success envelope.
func Created(result any) Response {
	return Response{Status: http.StatusCreated, Body: successEnvelope{Success: true, Result: result}}
}

// Fail is the error envelope {"success":false,"error":{"code","message",...extra}}.
func Fail(status int, code, message string, extra map[string]any) Response {
	e := map[string]any{"code": code, "message": message}
	for k, v := range extra {
		e[k] = v
	}
	return Response{Status: status, Body: map[string]any{"success": false, "error": e}}
}

// Legacy is the older error shape {"success":false,"result":...} some endpoints
// still use — partly with HTTP 200 (e.g. GET /rootserver/{id} for an unknown id).
func Legacy(status int, result any) Response {
	return Response{Status: status, Body: successEnvelope{Success: false, Result: result}}
}

// Raw sends body as JSON without an envelope.
func Raw(status int, body any) Response { return Response{Status: status, Body: body} }

// DropConnection closes the connection without an answer (a network error for the client).
func DropConnection() Response { return Response{drop: true} }

// DropAfterHandling, injected for a route, runs the real handler (state
// changes such as a charge happen) and then closes the connection, so the
// client never sees the answer — a response lost on the way back.
func DropAfterHandling() Response { return Response{drop: true, afterHandler: true} }

// WithHeader returns a copy of r with an added response header.
func (r Response) WithHeader(key, value string) Response {
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set(key, value)
	r.Header = h
	return r
}

// RecordedRequest is one request the server received.
type RecordedRequest struct {
	Method string
	Path   string
	// Pattern is the matched route, e.g. /orders/{uuid} ("" if none matched).
	Pattern string
	Header  http.Header
	Body    []byte
}

type route struct {
	method  string
	pattern string
	segs    []string
	handler HandlerFunc
}

// limiter is a fixed-window rate limit like the API's: the window starts with
// the first request, and once limit requests were let through, every further
// one is refused until the window has passed.
type limiter struct {
	limit  int
	window time.Duration
	// routes limits the limiter to "METHOD /pattern" keys; nil = all requests.
	routes map[string]bool

	start time.Time
	count int
}

// allow counts a request and returns how long to wait when it is refused.
func (l *limiter) allow(now time.Time) (bool, time.Duration) {
	if l.start.IsZero() || now.Sub(l.start) >= l.window {
		l.start, l.count = now, 0
	}
	if l.count >= l.limit {
		return false, l.window - now.Sub(l.start)
	}
	l.count++
	return true, 0
}

// tooManyRequests is the API's 429 answer, with Retry-After in whole seconds
// (rounded up) like the API.
func tooManyRequests(limit int, wait time.Duration) Response {
	secs := int(math.Ceil(wait.Seconds()))
	return Fail(http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "Zu viele Anfragen.", nil).
		WithHeader("Retry-After", strconv.Itoa(max(secs, 1))).
		WithHeader("X-RateLimit-Limit", strconv.Itoa(limit)).
		WithHeader("X-RateLimit-Remaining", "0")
}

type injected struct {
	remaining int
	resp      Response
}

// Server is the running mock. URL ends in /api like the real base URL.
type Server struct {
	URL   string
	Token string

	t  testing.TB
	ts *httptest.Server

	mu       sync.Mutex
	state    *State
	routes   []*route
	inject   map[string][]*injected
	requests []RecordedRequest

	throttle       *limiter
	routeThrottles []*limiter
	throttled      int
}

// registrations are the default endpoints; files of this package add theirs in init().
var registrations []func(*Server)

func register(fn func(*Server)) { registrations = append(registrations, fn) }

// New starts a mock API that is closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		Token:  DefaultToken,
		t:      t,
		state:  newState(),
		inject: map[string][]*injected{},
	}
	for _, fn := range registrations {
		fn(s)
	}
	s.ts = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	s.URL = s.ts.URL + "/api"
	t.Cleanup(s.ts.Close)
	return s
}

// Handle registers (or replaces) the handler for method + pattern. Patterns are
// paths without the /api prefix; {name} matches one path segment.
func (s *Server) Handle(method, pattern string, h HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pattern = normalize(pattern)
	for _, r := range s.routes {
		if r.method == method && r.pattern == pattern {
			r.handler = h
			return
		}
	}
	s.routes = append(s.routes, &route{method: method, pattern: pattern, segs: strings.Split(pattern, "/"), handler: h})
}

// InjectResponse makes the next `times` requests to method + path answer with
// resp instead of the handler (after the token check). path is either a
// concrete path (/orders/abc) or a route pattern (/orders/{uuid}).
func (s *Server) InjectResponse(method, path string, times int, resp Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + normalize(path)
	s.inject[key] = append(s.inject[key], &injected{remaining: times, resp: resp})
}

// Throttle limits all requests to limit per window, like the API's general
// limit (60 per minute). A refused request answers 429 TOO_MANY_REQUESTS with
// Retry-After. limit <= 0 switches the limit off again.
func (s *Server) Throttle(limit int, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle = nil
	if limit > 0 {
		s.throttle = &limiter{limit: limit, window: window}
	}
}

// ThrottleRoutes adds a limit shared by the given routes ("METHOD /pattern",
// e.g. "POST /orders/quote"), like the API's order limit (10 per minute for
// quote and order together). It applies in addition to Throttle.
func (s *Server) ThrottleRoutes(limit int, window time.Duration, routes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := map[string]bool{}
	for _, r := range routes {
		method, pattern, _ := strings.Cut(r, " ")
		keys[method+" "+normalize(pattern)] = true
	}
	s.routeThrottles = append(s.routeThrottles, &limiter{limit: limit, window: window, routes: keys})
}

// ThrottledCount counts the requests refused with 429 by Throttle or ThrottleRoutes.
func (s *Server) ThrottledCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.throttled
}

// With runs fn with the state locked — for seeding or inspecting anything the
// helper methods do not cover.
func (s *Server) With(fn func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.state)
}

// CallCount counts requests to method + path (concrete path or route pattern).
func (s *Server) CallCount(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	path = normalize(path)
	n := 0
	for _, r := range s.requests {
		if r.Method == method && (r.Path == path || r.Pattern == path) {
			n++
		}
	}
	return n
}

// Requests returns a copy of all requests received so far.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

// WriteCount counts all non-GET requests — handy to assert "no API writes".
func (s *Server) WriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Method != http.MethodGet {
			n++
		}
	}
	return n
}

func (s *Server) serveHTTP(w http.ResponseWriter, hr *http.Request) {
	body, _ := io.ReadAll(hr.Body)
	path := normalize(strings.TrimPrefix(hr.URL.Path, "/api"))

	s.mu.Lock()
	rt, params := s.match(hr.Method, path)
	rec := RecordedRequest{Method: hr.Method, Path: path, Header: hr.Header.Clone(), Body: body}
	if rt != nil {
		rec.Pattern = rt.pattern
	}
	s.requests = append(s.requests, rec)

	resp := s.dispatch(hr, rt, path, params, body)
	s.mu.Unlock()

	write(w, resp)
}

func (s *Server) dispatch(hr *http.Request, rt *route, path string, params map[string]string, body []byte) (resp Response) {
	if rt == nil {
		// No route matched, so no route middleware (limit, token check) runs —
		// like the real API. An injected response still wins.
		if inj, ok := s.takeInjected(hr.Method, path); ok && !inj.afterHandler {
			return inj
		}
		if s.pathExists(path) {
			return Fail(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", fmt.Sprintf("no route for %s %s", hr.Method, path), nil)
		}
		return Fail(http.StatusNotFound, "ENDPOINT_NOT_FOUND", fmt.Sprintf("no route for %s %s", hr.Method, path), nil)
	}

	now := time.Now()
	// The general limit runs first (middleware of the whole API group).
	if s.throttle != nil {
		if ok, wait := s.throttle.allow(now); !ok {
			s.throttled++
			return tooManyRequests(s.throttle.limit, wait)
		}
	}
	if hr.Header.Get("Authorization") != "Bearer "+s.Token {
		return Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "Unauthenticated.", nil)
	}
	for _, l := range s.routeThrottles {
		if !l.routes[hr.Method+" "+rt.pattern] {
			continue
		}
		if ok, wait := l.allow(now); !ok {
			s.throttled++
			return tooManyRequests(l.limit, wait)
		}
	}

	inj, injectedOK := s.takeInjected(hr.Method, path)
	if !injectedOK {
		inj, injectedOK = s.takeInjected(hr.Method, rt.pattern)
	}
	if injectedOK && !inj.afterHandler {
		return inj
	}

	defer func() {
		if p := recover(); p != nil {
			resp = Fail(http.StatusInternalServerError, "MOCK_PANIC", fmt.Sprint(p), nil)
		}
		if injectedOK && inj.afterHandler {
			resp = DropConnection()
		}
	}()
	return rt.handler(s.state, &Request{Request: hr, Path: path, Params: params, Body: body})
}

func (s *Server) takeInjected(method, path string) (Response, bool) {
	key := method + " " + path
	queue := s.inject[key]
	for len(queue) > 0 {
		head := queue[0]
		if head.remaining <= 0 {
			queue = queue[1:]
			continue
		}
		head.remaining--
		if head.remaining == 0 {
			queue = queue[1:]
		}
		s.inject[key] = queue
		return head.resp, true
	}
	delete(s.inject, key)
	return Response{}, false
}

// pathExists reports whether any method has a route for path.
func (s *Server) pathExists(path string) bool {
	for _, r := range s.routes {
		if rt, _ := s.match(r.method, path); rt != nil {
			return true
		}
	}
	return false
}

func (s *Server) match(method, path string) (*route, map[string]string) {
	segs := strings.Split(path, "/")
	for _, r := range s.routes {
		if r.method != method || len(r.segs) != len(segs) {
			continue
		}
		params := map[string]string{}
		ok := true
		for i, seg := range r.segs {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				params[seg[1:len(seg)-1]] = segs[i]
				continue
			}
			if seg != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return r, params
		}
	}
	return nil, nil
}

func write(w http.ResponseWriter, resp Response) {
	if resp.drop {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	payload, err := json.Marshal(resp.Body)
	if err != nil {
		status = http.StatusInternalServerError
		payload = []byte(`{"success":false,"error":{"code":"MOCK_ENCODING","message":"cannot encode response"}}`)
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func normalize(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	return "/" + strings.Trim(path, "/")
}

// round2 rounds money like the API (two decimals).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// nowRFC3339 is the timestamp format the mock uses.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
