// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package client_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

func rootserverItem(hostname string) map[string]any {
	return map[string]any{
		"type":             "rootpacket",
		"packet_id":        2,
		"duration_months":  1,
		"template_id":      12,
		"hostname":         hostname,
		"client_reference": hostname,
	}
}

func newTestClient(api *testapi.Server) *client.Client {
	return client.New(api.URL, api.Token, client.WithRetryBackoff(time.Millisecond), client.WithPollInterval(5*time.Millisecond))
}

func TestQuote(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetPrice("rootpacket", 9.99)

	q, err := newTestClient(api).Quote(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Total != 9.99 || q.Subtotal != 9.99 || q.Discount != 0 {
		t.Errorf("totals = %+v", q)
	}
	if q.Balance != 100 || !q.Affordable {
		t.Errorf("balance/affordable = %v/%v", q.Balance, q.Affordable)
	}
	if len(q.Items) != 1 || q.Items[0].Type != "rootpacket" || q.Items[0].DurationMonths != 1 || q.Items[0].Total != 9.99 {
		t.Errorf("items = %+v", q.Items)
	}
	if q.Items[0].PriceMonthly == nil || *q.Items[0].PriceMonthly != 9.99 {
		t.Errorf("price_monthly = %v", q.Items[0].PriceMonthly)
	}
	if api.ChargeCount() != 0 {
		t.Errorf("quote must not charge")
	}
}

func TestPlaceOrder_ChargesOnceAndReplays(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(50)
	api.SetPrice("rootpacket", 9.99)
	c := newTestClient(api)
	req := client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}

	first, err := c.PlaceOrder(context.Background(), req, 9.99, "tf-key-0001")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if first.ID == "" || first.Total != 9.99 || len(first.Items) != 1 || first.Replayed {
		t.Fatalf("order = %+v", first)
	}
	item := first.Items[0]
	if item.Type != "rootpacket" || item.ResourceType != "rootserver" || item.ResourceID == 0 || item.ClientReference != "web1" {
		t.Errorf("item = %+v", item)
	}

	again, err := c.PlaceOrder(context.Background(), req, 9.99, "tf-key-0001")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Replayed || again.ID != first.ID || again.Items[0].ResourceID != item.ResourceID {
		t.Errorf("replay = %+v, first = %+v", again, first)
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d, want 1", api.ChargeCount())
	}
	if got := api.Balance(); got != 40.01 {
		t.Errorf("balance = %v, want 40.01", got)
	}
}

func TestPlaceOrder_RejectsShortIdempotencyKeyLocally(t *testing.T) {
	api := testapi.New(t)
	_, err := newTestClient(api).PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("a")}}, 1, "short")
	if err == nil {
		t.Fatal("want error")
	}
	if api.CallCount(http.MethodPost, "/orders") != 0 {
		t.Errorf("request must not be sent")
	}
}

func TestPlaceOrder_InsufficientBalance(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(5)
	api.SetPrice("rootpacket", 9.5)

	_, err := newTestClient(api).PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.5, "tf-key-0002")
	apiErr, ok := client.AsAPIError(err)
	if !ok || apiErr.Status != http.StatusPaymentRequired || apiErr.Code != client.CodeInsufficientBalance {
		t.Fatalf("err = %v", err)
	}
	if v, _ := apiErr.Float("missing_amount"); v != 4.5 {
		t.Errorf("missing_amount = %v", v)
	}
	if api.ChargeCount() != 0 {
		t.Errorf("must not charge")
	}
}

func TestPlaceOrder_PriceChanged(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetPrice("rootpacket", 12)

	_, err := newTestClient(api).PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0003")
	apiErr, ok := client.AsAPIError(err)
	if !ok || apiErr.Code != client.CodePriceChanged {
		t.Fatalf("err = %v", err)
	}
	if exp, _ := apiErr.Float("expected"); exp != 9.99 {
		t.Errorf("expected = %v", exp)
	}
	if act, _ := apiErr.Float("actual"); act != 12 {
		t.Errorf("actual = %v", act)
	}
}

func TestPlaceOrder_ClientReferenceInUse(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	id := api.SeedResource("rootserver", map[string]any{
		"client_reference": "web1",
		"expires_at":       time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})

	_, err := newTestClient(api).PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0004")
	apiErr, ok := client.AsAPIError(err)
	if !ok || apiErr.Status != http.StatusConflict || apiErr.Code != client.CodeClientReferenceInUse {
		t.Fatalf("err = %v", err)
	}
	if got, _ := apiErr.Int64("resource_id"); got != id {
		t.Errorf("resource_id = %d, want %d", got, id)
	}
}

func TestPlaceOrder_RetriesInjected5xxWithSameKey(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 2, testapi.Fail(http.StatusInternalServerError, "ORDER_FAILED", "rolled back", nil))

	order, err := newTestClient(api).PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0005")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if order.ID == "" {
		t.Errorf("no order id")
	}
	if api.CallCount(http.MethodPost, "/orders") != 3 || api.ChargeCount() != 1 {
		t.Errorf("calls = %d, charges = %d", api.CallCount(http.MethodPost, "/orders"), api.ChargeCount())
	}
}

// The order is charged but the answer is lost on the way back: the client
// retries with the same key and gets the replay — no second charge.
func TestPlaceOrder_LostResponseIsReplayedNotChargedTwice(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 1, testapi.DropAfterHandling())

	order, err := newTestClient(api).PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-lost1")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if !order.Replayed || len(order.Items) != 1 || order.Items[0].ResourceID == 0 {
		t.Errorf("order = %+v, want replay", order)
	}
	if n := api.CallCount(http.MethodPost, "/orders"); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d, want 1", api.ChargeCount())
	}
	if got := api.Balance(); got != 90.01 {
		t.Errorf("balance = %v, want 90.01", got)
	}
}

func TestQuote_IsNotRetried(t *testing.T) {
	api := testapi.New(t)
	api.InjectResponse(http.MethodPost, "/orders/quote", 1, testapi.Fail(http.StatusServiceUnavailable, "UNAVAILABLE", "down", nil))

	_, err := newTestClient(api).Quote(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}})
	if err == nil {
		t.Fatal("want error")
	}
	if n := api.CallCount(http.MethodPost, "/orders/quote"); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestGetOrder_NotFound(t *testing.T) {
	api := testapi.New(t)
	_, err := newTestClient(api).GetOrder(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !client.IsCode(err, client.CodeOrderNotFound) || !client.IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitForItem_PollsUntilActive(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisioningPolls(3)
	c := newTestClient(api)

	order, err := c.PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0006")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if order.Items[0].Status != client.StatusProvisioning {
		t.Errorf("initial status = %q", order.Items[0].Status)
	}

	item, err := c.WaitForItem(context.Background(), order.ID, 0, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForItem: %v", err)
	}
	if item.Status != client.StatusActive || item.ResourceID != order.Items[0].ResourceID {
		t.Errorf("item = %+v", item)
	}
	if n := api.CallCount(http.MethodGet, "/orders/"+order.ID); n != 4 {
		t.Errorf("polls = %d, want 4", n)
	}
}

func TestWaitForItem_Failed(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisionOutcome(client.StatusFailed)
	c := newTestClient(api)

	order, err := c.PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0007")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	item, err := c.WaitForItem(context.Background(), order.ID, 0, time.Millisecond)
	if !errors.Is(err, client.ErrItemFailed) {
		t.Fatalf("err = %v, want ErrItemFailed", err)
	}
	if item == nil || item.Status != client.StatusFailed {
		t.Errorf("item = %+v", item)
	}
}

func TestWaitForItem_Timeout(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisioningPolls(1_000_000)
	c := newTestClient(api)

	order, err := c.PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0008")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	item, err := c.WaitForItem(ctx, order.ID, 0, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if item == nil || item.Status != client.StatusProvisioning {
		t.Errorf("last seen item = %+v", item)
	}
}

func TestWaitForItem_IndexOutOfRange(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	c := newTestClient(api)

	order, err := c.PlaceOrder(context.Background(), client.OrderRequest{Items: []map[string]any{rootserverItem("web1")}}, 9.99, "tf-key-0009")
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if _, err := c.WaitForItem(context.Background(), order.ID, 5, time.Millisecond); err == nil {
		t.Fatal("want error")
	}
}

func TestPollIntervalDefault(t *testing.T) {
	if got := client.New("https://example.invalid", "t").PollInterval(); got != 10*time.Second {
		t.Errorf("PollInterval = %v, want 10s", got)
	}
}
