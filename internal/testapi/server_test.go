// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   map[string]any  `json:"error"`
}

func call(t *testing.T, api *testapi.Server, method, path, token string, body any, headers map[string]string) (*http.Response, envelope) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, api.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%s %s: invalid JSON %q", method, path, raw)
	}
	return resp, env
}

func item(ref string) map[string]any {
	return map[string]any{"type": "rootserver", "template_id": 1, "hostname": ref, "client_reference": ref}
}

func TestURLHasAPIPrefix(t *testing.T) {
	api := testapi.New(t)
	if !strings.HasSuffix(api.URL, "/api") {
		t.Errorf("URL = %q", api.URL)
	}
}

func TestAuthIsRequired(t *testing.T) {
	api := testapi.New(t)
	resp, env := call(t, api, http.MethodGet, "/account/balance", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized || env.Success || env.Error["code"] != "UNAUTHENTICATED" {
		t.Errorf("status=%d env=%+v", resp.StatusCode, env)
	}
	resp, _ = call(t, api, http.MethodGet, "/account/balance", "wrong", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status=%d", resp.StatusCode)
	}
}

func TestBalance(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(42.5)
	resp, env := call(t, api, http.MethodGet, "/account/balance", api.Token, nil, nil)
	if resp.StatusCode != http.StatusOK || !env.Success || string(env.Result) != `{"balance":42.5,"currency":"EUR"}` {
		t.Errorf("status=%d result=%s", resp.StatusCode, env.Result)
	}
}

func TestUnknownRoute(t *testing.T) {
	api := testapi.New(t)
	resp, env := call(t, api, http.MethodGet, "/nope", api.Token, nil, nil)
	if resp.StatusCode != http.StatusNotFound || env.Success {
		t.Errorf("status=%d env=%+v", resp.StatusCode, env)
	}
}

func TestHandleCustomRouteWithParams(t *testing.T) {
	api := testapi.New(t)
	api.Handle(http.MethodGet, "/domain/{id}/dns/{recordId}", func(s *testapi.State, r *testapi.Request) testapi.Response {
		id, ok := r.ParamInt64("id")
		if !ok {
			return testapi.Fail(http.StatusBadRequest, "BAD", "bad id", nil)
		}
		return testapi.OK(map[string]any{"id": id, "record": r.Param("recordId"), "balance": s.Balance})
	})
	api.SetBalance(3)

	_, env := call(t, api, http.MethodGet, "/domain/9/dns/77", api.Token, nil, nil)
	if string(env.Result) != `{"balance":3,"id":9,"record":"77"}` {
		t.Errorf("result=%s", env.Result)
	}
}

func TestInjectResponseIsConsumedInOrder(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(1)
	api.InjectResponse(http.MethodGet, "/account/balance", 1, testapi.Fail(http.StatusBadGateway, "UPSTREAM", "x", map[string]any{"retry": true}))

	resp, env := call(t, api, http.MethodGet, "/account/balance", api.Token, nil, nil)
	if resp.StatusCode != http.StatusBadGateway || env.Error["code"] != "UPSTREAM" || env.Error["retry"] != true {
		t.Errorf("first: status=%d env=%+v", resp.StatusCode, env)
	}
	resp, _ = call(t, api, http.MethodGet, "/account/balance", api.Token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("second: status=%d", resp.StatusCode)
	}
	if n := api.CallCount(http.MethodGet, "/account/balance"); n != 2 {
		t.Errorf("CallCount = %d", n)
	}
}

func TestInjectResponseMatchesRoutePattern(t *testing.T) {
	api := testapi.New(t)
	api.InjectResponse(http.MethodGet, "/orders/{uuid}", 1, testapi.Legacy(http.StatusOK, "No Entrys available."))

	resp, env := call(t, api, http.MethodGet, "/orders/abc", api.Token, nil, nil)
	if resp.StatusCode != http.StatusOK || env.Success || string(env.Result) != `"No Entrys available."` {
		t.Errorf("status=%d env=%+v", resp.StatusCode, env)
	}
}

func TestDropConnection(t *testing.T) {
	api := testapi.New(t)
	api.InjectResponse(http.MethodGet, "/account/balance", 1, testapi.DropConnection())

	req, _ := http.NewRequest(http.MethodGet, api.URL+"/account/balance", nil)
	req.Header.Set("Authorization", "Bearer "+api.Token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("want network error")
	}
}

func TestPlaceOrder_ValidationAndIdempotency(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	body := map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}

	resp, env := call(t, api, http.MethodPost, "/orders", api.Token, body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "VALIDATION_FAILED" {
		t.Errorf("missing key: status=%d env=%+v", resp.StatusCode, env)
	}

	key := map[string]string{"Idempotency-Key": "key-12345678"}
	resp, _ = call(t, api, http.MethodPost, "/orders", api.Token, body, key)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: status=%d", resp.StatusCode)
	}
	resp, _ = call(t, api, http.MethodPost, "/orders", api.Token, body, key)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: status=%d header=%q", resp.StatusCode, resp.Header.Get("Idempotent-Replayed"))
	}

	other := map[string]any{"items": []any{item("web2")}, "expected_total": 9.99}
	resp, env = call(t, api, http.MethodPost, "/orders", api.Token, other, key)
	if resp.StatusCode != http.StatusConflict || env.Error["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Errorf("conflict: status=%d env=%+v", resp.StatusCode, env)
	}

	api.MarkIdempotencyInProgress("key-inprogress")
	resp, env = call(t, api, http.MethodPost, "/orders", api.Token, other, map[string]string{"Idempotency-Key": "key-inprogress"})
	if resp.StatusCode != http.StatusConflict || env.Error["code"] != "IDEMPOTENCY_IN_PROGRESS" {
		t.Errorf("in progress: status=%d env=%+v", resp.StatusCode, env)
	}

	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d", api.ChargeCount())
	}
}

func TestPlaceOrder_DuplicateReferenceInOneOrder(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	body := map[string]any{"items": []any{item("web1"), item("web1")}, "expected_total": 19.98}
	resp, env := call(t, api, http.MethodPost, "/orders", api.Token, body, map[string]string{"Idempotency-Key": "key-dup-0001"})
	if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "VALIDATION_FAILED" {
		t.Errorf("status=%d env=%+v", resp.StatusCode, env)
	}
}

func TestPlaceOrder_HostnameIsDefaultReference(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	it := map[string]any{"type": "rootserver", "template_id": 1, "hostname": "web9"}
	resp, env := call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{it}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-host-001"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(env.Result), `"client_reference":"web9"`) {
		t.Errorf("status=%d result=%s", resp.StatusCode, env.Result)
	}
}

func TestPlaceOrder_ExpiredResourceFreesReference(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SeedResource("rootserver", map[string]any{"client_reference": "web1", "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})

	resp, _ := call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-expired1"})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

func TestPlaceOrder_CreatesResourceWithDefaults(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisioner("rootserver", func(item, attrs map[string]any) {
		attrs["ipv4"] = []string{"192.0.2.10"}
	})

	call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-create01"})

	ids := api.ResourceIDs("rootserver")
	if len(ids) != 1 {
		t.Fatalf("ids = %v", ids)
	}
	res, ok := api.Resource("rootserver", ids[0])
	if !ok {
		t.Fatal("resource missing")
	}
	if res["client_reference"] != "web1" || res["hostname"] != "web1" || res["auto_renew"] != true || res["expires_at"] == nil || res["ipv4"] == nil {
		t.Errorf("resource = %v", res)
	}
	if res["id"] != ids[0] {
		t.Errorf("id attribute = %v", res["id"])
	}
}

func TestPromoCode(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetPrice("domain", 10)
	api.SetPromo("SAVE2", 2)
	body := map[string]any{"items": []any{map[string]any{"type": "domain", "sld": "example", "tld": "de"}}, "promo_code": "SAVE2"}

	_, env := call(t, api, http.MethodPost, "/orders/quote", api.Token, body, nil)
	var q struct {
		Subtotal, Discount, Total float64
		Items                     []struct {
			PriceMonthly *float64 `json:"price_monthly"`
		}
	}
	if err := json.Unmarshal(env.Result, &q); err != nil {
		t.Fatal(err)
	}
	if q.Subtotal != 10 || q.Discount != 2 || q.Total != 8 {
		t.Errorf("quote = %+v", q)
	}
	if q.Items[0].PriceMonthly != nil {
		t.Errorf("domain price_monthly must be null")
	}

	body["promo_code"] = "UNKNOWN"
	_, env = call(t, api, http.MethodPost, "/orders/quote", api.Token, body, nil)
	if err := json.Unmarshal(env.Result, &q); err != nil {
		t.Fatal(err)
	}
	if q.Total != 10 {
		t.Errorf("unknown promo must be ignored, total = %v", q.Total)
	}
}

func TestDomainUnavailable(t *testing.T) {
	api := testapi.New(t)
	api.SetDomainUnavailable("taken.de")
	body := map[string]any{"items": []any{map[string]any{"type": "domain", "sld": "Taken", "tld": "de"}}}
	resp, env := call(t, api, http.MethodPost, "/orders/quote", api.Token, body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "DOMAIN_UNAVAILABLE" || env.Error["domain"] != "taken.de" {
		t.Errorf("status=%d env=%+v", resp.StatusCode, env)
	}
	if _, ok := env.Error["field"]; ok {
		t.Errorf("DOMAIN_UNAVAILABLE carries only domain, got %+v", env.Error)
	}
}

func TestDomainRequiresSldAndTld(t *testing.T) {
	api := testapi.New(t)
	cases := map[string]struct {
		item  map[string]any
		field string
	}{
		"full name only": {map[string]any{"type": "domain", "name": "example.de"}, "items.0.sld"},
		"no tld":         {map[string]any{"type": "domain", "sld": "example"}, "items.0.tld"},
	}
	for name, tc := range cases {
		resp, env := call(t, api, http.MethodPost, "/orders/quote", api.Token, map[string]any{"items": []any{tc.item}}, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "VALIDATION_FAILED" || env.Error["field"] != tc.field {
			t.Errorf("%s: status=%d env=%+v", name, resp.StatusCode, env)
		}
	}
}

func TestDomainOrderStoresFullName(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	it := map[string]any{"type": "domain", "sld": "example", "tld": "de", "client_reference": "example.de"}
	resp, _ := call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{it}, "expected_total": 12.0}, map[string]string{"Idempotency-Key": "key-domain01"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	ids := api.ResourceIDs(testapi.ResourceDomain)
	res, _ := api.Resource(testapi.ResourceDomain, ids[0])
	if res["name"] != "example.de" || res["sld"] != "example" {
		t.Errorf("resource = %v", res)
	}
}

func TestGetOrder_ItemFailedWhenResourceGone(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisioningPolls(0)
	_, env := call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-gone0001"})
	var order struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(env.Result, &order)

	api.DeleteResource(testapi.ResourceRootserver, api.ResourceIDs(testapi.ResourceRootserver)[0])
	_, env = call(t, api, http.MethodGet, "/orders/"+order.ID, api.Token, nil, nil)
	if !strings.Contains(string(env.Result), `"status":"failed"`) {
		t.Errorf("result=%s", env.Result)
	}
}

func TestDropAfterHandlingRunsHandler(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 1, testapi.DropAfterHandling())

	b, _ := json.Marshal(map[string]any{"items": []any{item("web1")}, "expected_total": 9.99})
	req, _ := http.NewRequest(http.MethodPost, api.URL+"/orders", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+api.Token)
	req.Header.Set("Idempotency-Key", "key-drop0001")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("want network error")
	}
	if api.ChargeCount() != 1 {
		t.Errorf("handler must have run: charges = %d", api.ChargeCount())
	}
}

func TestSetOrderItemStatus(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	_, env := call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-status01"})
	var order struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(env.Result, &order)

	api.SetOrderItemStatus(order.ID, 0, "failed")
	_, env = call(t, api, http.MethodGet, "/orders/"+order.ID, api.Token, nil, nil)
	if !strings.Contains(string(env.Result), `"status":"failed"`) {
		t.Errorf("result=%s", env.Result)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A slow POST /orders: the order is charged, but the key keeps answering
// IDEMPOTENCY_IN_PROGRESS for a while (like the API while the first request
// still runs) before the completed order is replayed.
func TestPlaceOrder_SlowOrderAnswersInProgressThenReplays(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetSlowOrder(2)
	body := map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}
	key := map[string]string{"Idempotency-Key": "key-slow-0001"}

	resp, _ := call(t, api, http.MethodPost, "/orders", api.Token, body, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: status=%d", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		// In progress wins over a different body, as in OrderPlacementService.
		other := map[string]any{"items": []any{item("web2")}, "expected_total": 9.99}
		resp, env := call(t, api, http.MethodPost, "/orders", api.Token, other, key)
		if resp.StatusCode != http.StatusConflict || env.Error["code"] != "IDEMPOTENCY_IN_PROGRESS" {
			t.Fatalf("retry %d: status=%d env=%+v", i, resp.StatusCode, env)
		}
	}
	resp, _ = call(t, api, http.MethodPost, "/orders", api.Token, body, key)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("after: status=%d replayed=%q", resp.StatusCode, resp.Header.Get("Idempotent-Replayed"))
	}
	// The slow setting applies to one order only.
	resp, _ = call(t, api, http.MethodPost, "/orders", api.Token,
		map[string]any{"items": []any{item("web3")}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-slow-0002"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("next order: status=%d", resp.StatusCode)
	}
	resp, _ = call(t, api, http.MethodPost, "/orders", api.Token,
		map[string]any{"items": []any{item("web3")}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-slow-0002"})
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("next order replay: status=%d", resp.StatusCode)
	}
	if api.ChargeCount() != 2 {
		t.Errorf("charges = %d, want 2", api.ChargeCount())
	}
}

// The request fingerprint is normalised like OrderPlacementService::requestHash:
// expected_total to two decimals, promo_code trimmed and "" = absent.
func TestPlaceOrder_FingerprintIsNormalised(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	key := map[string]string{"Idempotency-Key": "key-norm-0001"}

	resp, _ := call(t, api, http.MethodPost, "/orders", api.Token,
		map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: status=%d", resp.StatusCode)
	}
	resp, env := call(t, api, http.MethodPost, "/orders", api.Token,
		map[string]any{"items": []any{item("web1")}, "expected_total": 9.990, "promo_code": "  "}, key)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("normalised replay: status=%d env=%+v", resp.StatusCode, env)
	}
	resp, env = call(t, api, http.MethodPost, "/orders", api.Token,
		map[string]any{"items": []any{item("web1")}, "expected_total": 10.5}, key)
	if resp.StatusCode != http.StatusConflict || env.Error["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("other total: status=%d env=%+v", resp.StatusCode, env)
	}
}

func TestSetOrderCreatedAtBackdatesOrderAndReplay(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	body := map[string]any{"items": []any{item("web1")}, "expected_total": 9.99}
	key := map[string]string{"Idempotency-Key": "key-backdate-1"}
	if resp, _ := call(t, api, http.MethodPost, "/orders", api.Token, body, key); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ids := api.OrderIDs()
	if len(ids) != 1 {
		t.Fatalf("orders = %v", ids)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	api.SetOrderCreatedAt(ids[0], at)

	var order struct {
		CreatedAt string `json:"created_at"`
	}
	_, env := call(t, api, http.MethodPost, "/orders", api.Token, body, key)
	if err := json.Unmarshal(env.Result, &order); err != nil || order.CreatedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("replay created_at = %q (%v)", order.CreatedAt, err)
	}
	_, env = call(t, api, http.MethodGet, "/orders/"+ids[0], api.Token, nil, nil)
	if err := json.Unmarshal(env.Result, &order); err != nil || order.CreatedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("GET created_at = %q (%v)", order.CreatedAt, err)
	}
}
