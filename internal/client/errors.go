// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Error codes the ordering endpoints return in error.code.
const (
	CodeValidationFailed      = "VALIDATION_FAILED"
	CodeDomainUnavailable     = "DOMAIN_UNAVAILABLE"
	CodeInsufficientBalance   = "INSUFFICIENT_BALANCE"
	CodePriceChanged          = "PRICE_CHANGED"
	CodeIdempotencyConflict   = "IDEMPOTENCY_CONFLICT"
	CodeIdempotencyInProgress = "IDEMPOTENCY_IN_PROGRESS"
	CodeClientReferenceInUse  = "CLIENT_REFERENCE_IN_USE"
	CodePromoCodeUnavailable  = "PROMO_CODE_UNAVAILABLE"
	CodeOrderFailed           = "ORDER_FAILED"
	CodeOrderNotFound         = "ORDER_NOT_FOUND"
	CodeInsufficientScope     = "INSUFFICIENT_SCOPE"
	CodeDuplicateContact      = "DUPLICATE_CONTACT"
	CodeUnauthenticated       = "UNAUTHENTICATED"
	CodeTooManyRequests       = "TOO_MANY_REQUESTS"
	// CodeEndpointNotFound is the answer to a path the API has no route for
	// (HTTP 404) — a wrong endpoint or an API without that route, never a
	// missing resource. A resource that does not exist is NOT_FOUND (or a
	// more specific *_NOT_FOUND code).
	CodeEndpointNotFound = "ENDPOINT_NOT_FOUND"
)

// APIError is a failed API call: either the error envelope
// {"success":false,"error":{"code","message",...}} or, for older endpoints,
// {"success":false,"result":"<message>"} (which may come with HTTP 200).
//
// Extra holds every field of the error object besides code and message,
// e.g. missing_amount, expected/actual, resource_type/resource_id.
type APIError struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any

	// Enveloped is true when the body was a valid API envelope
	// ({"success":false,...}). False means the answer came from something else
	// (proxy, wrong endpoint, HTML error page) and says nothing about resources.
	Enveloped bool
}

// Error implements error.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "prepaid-host API: HTTP %d", e.Status)
	if e.Code != "" {
		b.WriteString(" ")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Float returns a numeric field of Extra.
func (e *APIError) Float(key string) (float64, bool) {
	switch v := e.Extra[key].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// Int64 returns an integral numeric field of Extra.
func (e *APIError) Int64(key string) (int64, bool) {
	f, ok := e.Float(key)
	if !ok || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// String returns a string field of Extra.
func (e *APIError) String(key string) (string, bool) {
	s, ok := e.Extra[key].(string)
	return s, ok
}

// AsAPIError unwraps err to an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	apiErr, ok := AsAPIError(err)
	return ok && apiErr.Code == code
}

// IsNotFound reports whether err means "this resource does not exist (any more)".
// Only answers of the API itself count (Enveloped): HTTP 404 with an envelope,
// or the legacy endpoints' {"success":false,"result":"No Entrys available."} /
// "... not found." at HTTP 200 or 404. An HTML 404 from a proxy or a wrong
// endpoint, a 404 ENDPOINT_NOT_FOUND (no such route — wrong endpoint), or
// "... not found." at another status (e.g. 422 "Template not found."), is
// not — a Read must not drop state because of it.
func IsNotFound(err error) bool {
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.Enveloped || apiErr.Code == CodeEndpointNotFound {
		return false
	}
	if apiErr.Status == 404 {
		return true
	}
	if apiErr.Status != 200 || apiErr.Code != "" {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "no entrys available") || strings.Contains(msg, "not found")
}
