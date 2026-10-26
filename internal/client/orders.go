// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Status of an order and of its items (GET /orders/{uuid}).
const (
	StatusProvisioning = "provisioning"
	StatusActive       = "active"
	StatusFailed       = "failed"
)

// ErrItemFailed is returned by WaitForItem when the item ended in status "failed".
var ErrItemFailed = errors.New("order item failed")

// OrderRequest is the body of POST /orders/quote and POST /orders.
// Each item carries "type" (rootserver|rootpacket|domain) plus the product
// fields of that type, see GET /catalog/{type}.
type OrderRequest struct {
	Items     []map[string]any `json:"items"`
	PromoCode string           `json:"promo_code,omitempty"`
}

// QuoteItem is one position of a Quote.
type QuoteItem struct {
	Type string `json:"type"`
	Name string `json:"name"`
	// PriceMonthly is nil for domains (their price covers the whole period).
	PriceMonthly   *float64 `json:"price_monthly"`
	DurationMonths int      `json:"duration_months"`
	Total          float64  `json:"total"`
}

// Quote is the result of POST /orders/quote. Total is the binding amount
// that belongs into expected_total of the order.
type Quote struct {
	Items      []QuoteItem `json:"items"`
	Subtotal   float64     `json:"subtotal"`
	Discount   float64     `json:"discount"`
	Total      float64     `json:"total"`
	Balance    float64     `json:"balance"`
	Affordable bool        `json:"affordable"`
}

// OrderItem is one position of an Order.
type OrderItem struct {
	Type            string `json:"type"`
	ResourceType    string `json:"resource_type"`
	ResourceID      int64  `json:"resource_id"`
	ClientReference string `json:"client_reference"`
	Status          string `json:"status"`
}

// Order is the result of POST /orders and GET /orders/{uuid}.
type Order struct {
	ID        string      `json:"id"`
	Status    string      `json:"status"`
	Total     float64     `json:"total"`
	CreatedAt string      `json:"created_at"`
	Items     []OrderItem `json:"items"`

	// Replayed is true when POST /orders answered with Idempotent-Replayed: true,
	// i.e. the order already existed for this Idempotency-Key and nothing was charged again.
	Replayed bool `json:"-"`
}

// Quote prices an order without charging anything.
func (c *Client) Quote(ctx context.Context, req OrderRequest) (*Quote, error) {
	var q Quote
	if err := c.Do(ctx, http.MethodPost, "/orders/quote", req, &q, nil); err != nil {
		return nil, err
	}
	return &q, nil
}

type placeOrderBody struct {
	OrderRequest
	ExpectedTotal float64 `json:"expected_total"`
}

// PlaceOrder charges the balance and creates all items. expectedTotal must
// equal the quote's Total (else 409 PRICE_CHANGED, nothing charged).
// idempotencyKey (8–255 characters) makes the call safe to repeat: the same
// key with the same body replays the original order instead of charging again.
func (c *Client) PlaceOrder(ctx context.Context, req OrderRequest, expectedTotal float64, idempotencyKey string) (*Order, error) {
	if n := len(idempotencyKey); n < 8 || n > 255 {
		return nil, fmt.Errorf("idempotency key must be 8 to 255 characters, got %d", n)
	}

	var o Order
	header, err := c.do(ctx, http.MethodPost, "/orders", placeOrderBody{OrderRequest: req, ExpectedTotal: expectedTotal}, &o,
		map[string]string{"Idempotency-Key": idempotencyKey})
	if err != nil {
		return nil, err
	}
	o.Replayed = header.Get("Idempotent-Replayed") == "true"
	return &o, nil
}

// GetOrder returns an order with the current status of each item.
func (c *Client) GetOrder(ctx context.Context, uuid string) (*Order, error) {
	var o Order
	if err := c.Do(ctx, http.MethodGet, "/orders/"+url.PathEscape(uuid), nil, &o, nil); err != nil {
		return nil, err
	}
	return &o, nil
}

// ListOrders returns one page of the account's orders, newest first
// (GET /orders). perPage is capped by the API at 100; a page without orders
// is the end of the list.
func (c *Client) ListOrders(ctx context.Context, page, perPage int) ([]Order, error) {
	var orders []Order
	path := fmt.Sprintf("/orders?page=%d&per_page=%d", max(page, 1), max(perPage, 1))
	if err := c.Do(ctx, http.MethodGet, path, nil, &orders, nil); err != nil {
		return nil, err
	}
	return orders, nil
}

// WaitForItem polls GET /orders/{uuid} every poll interval until item index
// is active. It returns ErrItemFailed (with the item) when the item failed,
// and the context's error (with the last seen item) on cancellation or
// timeout — bound the wait with context.WithTimeout. A poll <= 0 uses
// PollInterval().
func (c *Client) WaitForItem(ctx context.Context, uuid string, index int, poll time.Duration) (*OrderItem, error) {
	if poll <= 0 {
		poll = c.pollInterval
	}

	var last *OrderItem
	for {
		order, err := c.GetOrder(ctx, uuid)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return last, fmt.Errorf("waiting for order %s: %w", uuid, ctxErr)
			}
			return last, err
		}
		if index < 0 || index >= len(order.Items) {
			return nil, fmt.Errorf("order %s has %d items, no item %d", uuid, len(order.Items), index)
		}

		item := order.Items[index]
		last = &item
		switch item.Status {
		case StatusActive:
			return last, nil
		case StatusFailed:
			return last, fmt.Errorf("order %s item %d (%s %d): %w", uuid, index, item.ResourceType, item.ResourceID, ErrItemFailed)
		}

		if err := sleep(ctx, poll); err != nil {
			return last, fmt.Errorf("waiting for order %s: %w", uuid, err)
		}
	}
}
