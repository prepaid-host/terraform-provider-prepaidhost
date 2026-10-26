// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

const orderTestTimeout = 5 * time.Second

func orderTestClient(api *testapi.Server) *client.Client {
	return client.New(api.URL, api.Token, client.WithRetryBackoff(time.Millisecond), client.WithPollInterval(10*time.Millisecond))
}

func serverItem() map[string]any {
	return map[string]any{
		"type":          "rootserver",
		"cores":         2,
		"memory":        4,
		"disk":          50,
		"addresses":     1,
		"backups":       0,
		"traffic":       1000,
		"duration_days": 30,
		"template_id":   7,
		"hostname":      "web1.example.com",
		"password":      "Sup3r-Secret!",
		"ssh_key_ids":   []int64{3, 4},
	}
}

func domainItem() map[string]any {
	return map[string]any{
		"type":            "domain",
		"sld":             "example",
		"tld":             "de",
		"duration_months": 12,
		"action":          "register",
		"contact_id":      12,
	}
}

func errorText(diags diag.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags.Errors() {
		b.WriteString(d.Summary())
		b.WriteString("\n")
		b.WriteString(d.Detail())
		b.WriteString("\n")
	}
	return b.String()
}

func requireNoErrors(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if diags.HasError() {
		t.Fatalf("unexpected errors:\n%s", errorText(diags))
	}
}

func requireErrorContaining(t *testing.T, diags diag.Diagnostics, parts ...string) {
	t.Helper()
	if !diags.HasError() {
		t.Fatalf("expected an error containing %q, got none", parts)
	}
	text := errorText(diags)
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Errorf("error does not contain %q:\n%s", p, text)
		}
	}
}

func mustKey(t *testing.T, typ, ref string, item map[string]any) string {
	t.Helper()
	key, err := idempotencyKey(typ, ref, item)
	if err != nil {
		t.Fatalf("idempotencyKey: %v", err)
	}
	return key
}

func orderKeys(api *testapi.Server) []string {
	var keys []string
	for _, r := range api.Requests() {
		if r.Method == http.MethodPost && r.Path == "/orders" {
			keys = append(keys, r.Header.Get("Idempotency-Key"))
		}
	}
	return keys
}

func lastOrderBody(t *testing.T, api *testapi.Server) map[string]any {
	t.Helper()
	var body map[string]any
	for _, r := range api.Requests() {
		if r.Method == http.MethodPost && r.Path == "/orders" {
			body = map[string]any{}
			if err := json.Unmarshal(r.Body, &body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if body == nil {
		t.Fatal("no POST /orders")
	}
	return body
}

func TestOrderResource_Success(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisioningPolls(2)
	c := orderTestClient(api)

	id, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if adopted {
		t.Error("adopted = true for a new order")
	}
	if ids := api.ResourceIDs(testapi.ResourceRootserver); len(ids) != 1 || ids[0] != id {
		t.Fatalf("id = %d, resources = %v", id, ids)
	}
	if api.ChargeCount() != 1 || api.Balance() != 90.01 {
		t.Errorf("charges = %d, balance = %v", api.ChargeCount(), api.Balance())
	}
	if n := api.CallCount(http.MethodPost, "/orders/quote"); n != 1 {
		t.Errorf("quotes = %d", n)
	}
	if n := api.CallCount(http.MethodGet, "/orders/{uuid}"); n < 3 {
		t.Errorf("polls = %d, want >= 3 (2x provisioning, then active)", n)
	}

	keys := orderKeys(api)
	if want := mustKey(t, "rootserver", "web1", serverItem()); len(keys) != 1 || keys[0] != want {
		t.Errorf("keys = %v, want [%s]", keys, want)
	}
	body := lastOrderBody(t, api)
	if body["expected_total"] != 9.99 {
		t.Errorf("expected_total = %v", body["expected_total"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	item, _ := items[0].(map[string]any)
	if item["client_reference"] != "web1" || item["password"] != "Sup3r-Secret!" || item["cores"] != float64(2) {
		t.Errorf("item = %v", item)
	}
}

func TestOrderResource_Domain(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	c := orderTestClient(api)

	id, adopted, diags := orderResource(context.Background(), c, domainItem(), "example.de", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if adopted || id == 0 {
		t.Fatalf("id = %d adopted = %v", id, adopted)
	}
	if attrs, ok := api.Resource(testapi.ResourceDomain, id); !ok || attrs["name"] != "example.de" {
		t.Errorf("domain = %v", attrs)
	}
}

// The same configuration ordered again (e.g. apply after an aborted apply,
// or after destroy) replays the first order: same resource, no second charge.
// The replayed resource is reported as adopted, so the caller re-enables
// auto-renew (destroy switched it off).
func TestOrderResource_ReplaySameKey(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	c := orderTestClient(api)

	first, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	second, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)

	if first != second {
		t.Errorf("second order gave resource %d, first %d", second, first)
	}
	if !adopted {
		t.Error("a replayed order must be reported as adopted")
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d, want 1", api.ChargeCount())
	}
	if keys := orderKeys(api); len(keys) != 2 || keys[0] != keys[1] {
		t.Errorf("keys = %v", keys)
	}
}

// Charged, but the answer never arrived: the client retries with the same key
// and gets the replay.
func TestOrderResource_LostResponseIsReplayed(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 1, testapi.DropAfterHandling())
	c := orderTestClient(api)

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if ids := api.ResourceIDs(testapi.ResourceRootserver); len(ids) != 1 || ids[0] != id {
		t.Fatalf("id = %d, resources = %v", id, ids)
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d, want 1", api.ChargeCount())
	}
	if keys := orderKeys(api); len(keys) != 2 || keys[0] != keys[1] {
		t.Errorf("keys = %v", keys)
	}
}

func TestOrderResource_InProgressTwiceThenSuccess(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 2,
		testapi.Fail(http.StatusConflict, client.CodeIdempotencyInProgress, "Eine Anfrage mit diesem Idempotency-Key läuft noch.", nil))
	c := orderTestClient(api)

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if id == 0 || api.ChargeCount() != 1 {
		t.Fatalf("id = %d, charges = %d", id, api.ChargeCount())
	}
	keys := orderKeys(api)
	want := mustKey(t, "rootserver", "web1", serverItem())
	if len(keys) != 3 {
		t.Fatalf("POST /orders = %d, want 3", len(keys))
	}
	for i, k := range keys {
		if k != want {
			t.Errorf("request %d used key %q, want %q", i, k, want)
		}
	}
}

// A POST that outlives the HTTP timeout: the first request is charged but its
// answer is lost, the retries see IDEMPOTENCY_IN_PROGRESS until the first
// request finished, then the order is replayed — charged once, one key.
func TestOrderResource_SlowOrderIsWaitedFor(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetSlowOrder(3)
	api.InjectResponse(http.MethodPost, "/orders", 1, testapi.DropAfterHandling())
	c := orderTestClient(api)

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if ids := api.ResourceIDs(testapi.ResourceRootserver); len(ids) != 1 || ids[0] != id {
		t.Fatalf("id = %d, resources = %v", id, ids)
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d, want 1", api.ChargeCount())
	}
	keys := orderKeys(api)
	if len(keys) != 5 { // lost + 3x in progress + replay
		t.Errorf("POST /orders = %d, want 5", len(keys))
	}
	for i, k := range keys {
		if k != keys[0] {
			t.Errorf("request %d used key %q, want %q", i, k, keys[0])
		}
	}
}

func TestOrderResource_InProgressUntilTimeout(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	key := mustKey(t, "rootserver", "web1", serverItem())
	api.MarkIdempotencyInProgress(key)
	c := orderTestClient(api)

	id, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, 200*time.Millisecond)
	requireErrorContaining(t, diags, "Run apply again", "will not be bought twice", "IDEMPOTENCY_IN_PROGRESS")
	if id != 0 || adopted {
		t.Errorf("id = %d adopted = %v", id, adopted)
	}
	keys := orderKeys(api)
	if len(keys) < 2 {
		t.Errorf("POST /orders = %d, want several attempts", len(keys))
	}
	for i, k := range keys {
		if k != key {
			t.Errorf("request %d used key %q, want %q", i, k, key)
		}
	}
	if api.ChargeCount() != 0 {
		t.Errorf("charges = %d", api.ChargeCount())
	}
}

// Review Focus 1: apply aborted while provisioning (after the charge), the
// next apply with the same configuration takes the paid order over.
func TestOrderResource_TimeoutWhileProvisioningThenReapply(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisioningPolls(1_000_000)
	c := orderTestClient(api)

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, 150*time.Millisecond)
	requireErrorContaining(t, diags, "Run apply again", "will not be bought twice")
	ids := api.ResourceIDs(testapi.ResourceRootserver)
	if len(ids) != 1 || id != ids[0] {
		t.Fatalf("id = %d, resources = %v", id, ids)
	}
	if text := errorText(diags); !strings.Contains(text, "rootserver") || !strings.Contains(text, "already paid") {
		t.Errorf("timeout error should name the paid resource:\n%s", text)
	}

	// Provisioning finishes in the meantime.
	for _, o := range ordersOf(api) {
		api.SetOrderItemStatus(o, 0, client.StatusActive)
	}

	again, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if again != id || !adopted {
		t.Errorf("re-apply: id = %d (want %d), adopted = %v", again, id, adopted)
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d, want 1", api.ChargeCount())
	}
}

func ordersOf(api *testapi.Server) []string {
	var ids []string
	api.With(func(st *testapi.State) {
		for id := range st.Orders {
			ids = append(ids, id)
		}
	})
	return ids
}

func TestOrderResource_PriceChanged(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 1, testapi.Fail(http.StatusConflict, client.CodePriceChanged, "Der Preis hat sich geändert.",
		map[string]any{"expected": 9.99, "actual": 12.49}))
	c := orderTestClient(api)

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "PRICE_CHANGED", "9.99", "12.49", "Nothing was charged")
	if id != 0 || api.ChargeCount() != 0 {
		t.Errorf("id = %d, charges = %d", id, api.ChargeCount())
	}
	if n := api.CallCount(http.MethodPost, "/orders"); n != 1 {
		t.Errorf("POST /orders = %d, want 1 (no automatic re-order at a new price)", n)
	}
}

// Review Focus 4: not enough balance — the missing amount, the hint, nothing created.
func TestOrderResource_InsufficientBalance(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(5)
	c := orderTestClient(api)

	id, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "INSUFFICIENT_BALANCE", "4.99", "Top up", "customer panel")
	if id != 0 || adopted {
		t.Errorf("id = %d adopted = %v", id, adopted)
	}
	if api.ChargeCount() != 0 || len(api.ResourceIDs(testapi.ResourceRootserver)) != 0 {
		t.Errorf("charges = %d, resources = %v", api.ChargeCount(), api.ResourceIDs(testapi.ResourceRootserver))
	}
}

func TestOrderResource_FailedItem(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetProvisionOutcome(client.StatusFailed)
	c := orderTestClient(api)

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "failed", "support")
	if id == 0 {
		t.Error("a failed item still names the resource id")
	}
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d", api.ChargeCount())
	}
}

func TestOrderResource_MaxPriceExceededOrdersNothing(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	c := orderTestClient(api)
	maxPrice := 5.0

	id, _, diags := orderResource(context.Background(), c, serverItem(), "web1", &maxPrice, orderTestTimeout)
	requireErrorContaining(t, diags, "max_price", "9.99", "5.00")
	if id != 0 {
		t.Errorf("id = %d", id)
	}
	if n := api.CallCount(http.MethodPost, "/orders"); n != 0 {
		t.Errorf("POST /orders = %d, want 0", n)
	}
	if n := api.CallCount(http.MethodPost, "/orders/quote"); n != 1 {
		t.Errorf("quotes = %d", n)
	}
}

func TestOrderResource_MaxPriceEqualIsAllowed(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	c := orderTestClient(api)
	maxPrice := 9.99

	_, _, diags := orderResource(context.Background(), c, serverItem(), "web1", &maxPrice, orderTestTimeout)
	requireNoErrors(t, diags)
	if api.ChargeCount() != 1 {
		t.Errorf("charges = %d", api.ChargeCount())
	}
}

func TestOrderResource_ClientReferenceInUseAdopts(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	existing := api.SeedResource(testapi.ResourceRootserver, map[string]any{
		"client_reference": "web1",
		"expires_at":       time.Now().Add(240 * time.Hour).UTC().Format(time.RFC3339),
	})
	c := orderTestClient(api)

	id, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireNoErrors(t, diags)
	if id != existing || !adopted {
		t.Errorf("id = %d (want %d), adopted = %v", id, existing, adopted)
	}
	if api.ChargeCount() != 0 {
		t.Errorf("charges = %d", api.ChargeCount())
	}
}

func TestOrderResource_ClientReferenceInUseByOtherTypeIsAnError(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.InjectResponse(http.MethodPost, "/orders", 1, testapi.Fail(http.StatusConflict, client.CodeClientReferenceInUse, "Die Kennung ist bereits vergeben.",
		map[string]any{"resource_type": "domain", "resource_id": 77}))
	c := orderTestClient(api)

	id, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "CLIENT_REFERENCE_IN_USE", "domain", "77")
	if id != 0 || adopted {
		t.Errorf("id = %d adopted = %v", id, adopted)
	}
}

func TestOrderResource_ErrorDiagnostics(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		resp  testapi.Response
		times int
		want  []string
	}{
		{"terms", "/orders/quote", testapi.Fail(http.StatusForbidden, "TERMS_NOT_ACCEPTED", "No consent to the current ordering terms exists for this token.", nil), 1,
			[]string{"TERMS_NOT_ACCEPTED", "new API token in the customer panel", "accept the terms"}},
		{"scope", "/orders/quote", testapi.Fail(http.StatusForbidden, "INSUFFICIENT_SCOPE", "The token is missing the scope 'rootserver:order'.",
			map[string]any{"required_scope": "rootserver:order"}), 1,
			[]string{"INSUFFICIENT_SCOPE", "rootserver:order", "customer panel"}},
		{"bearer", "/orders", testapi.Fail(http.StatusForbidden, "ORDER_REQUIRES_BEARER_TOKEN", "Orders require a Bearer token.", nil), 1,
			[]string{"ORDER_REQUIRES_BEARER_TOKEN", "personal API token"}},
		{"account", "/orders", testapi.Fail(http.StatusForbidden, "ACCOUNT_NOT_ACTIVE", "Your account is not active.", nil), 1,
			[]string{"ACCOUNT_NOT_ACTIVE", "support"}},
		{"promo", "/orders", testapi.Fail(http.StatusConflict, "PROMO_CODE_UNAVAILABLE", "Der Promo-Code ist nicht mehr verfügbar.", nil), 1,
			[]string{"PROMO_CODE_UNAVAILABLE", "promo_code", "Nothing was charged"}},
		{"validation", "/orders/quote", testapi.Fail(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "The selected cores is invalid.",
			map[string]any{"field": "items.0.cores"}), 1,
			[]string{"VALIDATION_FAILED", `"cores"`, "The selected cores is invalid."}},
		{"conflict", "/orders", testapi.Fail(http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Der Idempotency-Key wurde mit anderem Inhalt verwendet.", nil), 1,
			[]string{"IDEMPOTENCY_CONFLICT", "terraform import", "Nothing was charged"}},
		{"order failed", "/orders", testapi.Fail(http.StatusInternalServerError, "ORDER_FAILED", "Die Bestellung ist fehlgeschlagen.", nil), 4,
			[]string{"ORDER_FAILED", "rolled back", "Run apply again"}},
		// Go's transport itself repeats a POST carrying an Idempotency-Key once on a
		// broken keep-alive connection, so inject more drops than client attempts.
		{"network", "/orders", testapi.DropConnection(), 20,
			[]string{"Run apply again", "will not be bought twice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := testapi.New(t)
			api.SetBalance(100)
			api.InjectResponse(http.MethodPost, tc.path, tc.times, tc.resp)
			c := orderTestClient(api)

			id, adopted, diags := orderResource(context.Background(), c, serverItem(), "web1", nil, orderTestTimeout)
			requireErrorContaining(t, diags, tc.want...)
			if id != 0 || adopted || api.ChargeCount() != 0 {
				t.Errorf("id = %d adopted = %v charges = %d", id, adopted, api.ChargeCount())
			}
		})
	}
}

func TestOrderResource_DomainUnavailable(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	api.SetDomainUnavailable("example.de")
	c := orderTestClient(api)

	_, _, diags := orderResource(context.Background(), c, domainItem(), "example.de", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "DOMAIN_UNAVAILABLE", "example.de", "transfer")
	if api.CallCount(http.MethodPost, "/orders") != 0 {
		t.Error("quote failed, nothing must be ordered")
	}
}

func TestOrderResource_RejectsBadInput(t *testing.T) {
	api := testapi.New(t)
	c := orderTestClient(api)

	_, _, diags := orderResource(context.Background(), c, serverItem(), "", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "client reference")
	bad := serverItem()
	bad["type"] = "vps"
	_, _, diags = orderResource(context.Background(), c, bad, "web1", nil, orderTestTimeout)
	requireErrorContaining(t, diags, "vps")
	if len(api.Requests()) != 0 {
		t.Errorf("requests = %d, want 0", len(api.Requests()))
	}
}

func TestIdempotencyKey_Format(t *testing.T) {
	key := mustKey(t, "rootserver", "web1", serverItem())
	if !strings.HasPrefix(key, "tf-") || len(key) != 3+64 {
		t.Fatalf("key = %q", key)
	}
	if strings.Contains(key, "Sup3r") {
		t.Fatal("key leaks the password")
	}
}

func TestIdempotencyKey_IsDeterministic(t *testing.T) {
	a := mustKey(t, "rootserver", "web1", serverItem())

	// Same content in another insertion order and with other number types.
	b := map[string]any{}
	keys := []string{"template_id", "traffic", "type", "hostname", "duration_days", "disk", "backups", "addresses", "memory", "cores"}
	src := serverItem()
	for _, k := range keys {
		b[k] = src[k]
	}
	b["cores"] = int64(2)
	b["memory"] = 4.0
	b["disk"] = json.Number("50")
	if got := mustKey(t, "rootserver", "web1", b); got != a {
		t.Errorf("reordered/retyped item gave %s, want %s", got, a)
	}

	// Fixed value: a key computed by another process (or provider version)
	// for the same input must stay the same — changing it breaks replays of
	// aborted applies. Computed independently:
	//   printf 'rootserver\0web1\0{"addresses":1,"backups":0,"cores":2,"disk":50,"duration_days":30,"memory":4,"template_id":7,"traffic":1000}' | shasum -a 256
	const golden = "tf-d56d3e2485d7af03ceb4ca9db9ffd899f37fa09b7b737eb2df4ea965798b3db0"
	if a != golden {
		t.Fatalf("key = %s, want %s", a, golden)
	}
}

func TestIdempotencyKey_RelevantFieldsChangeTheKey(t *testing.T) {
	base := mustKey(t, "rootserver", "web1", serverItem())
	for field, value := range map[string]any{
		"cores": 4, "memory": 8, "disk": 100, "addresses": 2, "backups": 1, "traffic": 2000, "duration_days": 90, "template_id": 8,
	} {
		item := serverItem()
		item[field] = value
		if mustKey(t, "rootserver", "web1", item) == base {
			t.Errorf("changing %s did not change the key", field)
		}
	}
	if mustKey(t, "rootserver", "web2", serverItem()) == base {
		t.Error("another client reference must give another key")
	}
	if mustKey(t, "rootpacket", "web1", serverItem()) == base {
		t.Error("another type must give another key")
	}

	packet := map[string]any{"type": "rootpacket", "packet_id": 3, "duration_months": 1, "template_id": 7}
	pbase := mustKey(t, "rootpacket", "web1", packet)
	for field, value := range map[string]any{"packet_id": 4, "duration_months": 12, "template_id": 9, "promo_code": "SPRING"} {
		item := map[string]any{"type": "rootpacket", "packet_id": 3, "duration_months": 1, "template_id": 7}
		item[field] = value
		if mustKey(t, "rootpacket", "web1", item) == pbase {
			t.Errorf("rootpacket: changing %s did not change the key", field)
		}
	}

	dbase := mustKey(t, "domain", "example.de", domainItem())
	for field, value := range map[string]any{"sld": "other", "tld": "com", "duration_months": 24, "action": "transfer"} {
		item := domainItem()
		item[field] = value
		if mustKey(t, "domain", "example.de", item) == dbase {
			t.Errorf("domain: changing %s did not change the key", field)
		}
	}
}

func TestIdempotencyKey_OtherFieldsKeepTheKey(t *testing.T) {
	base := mustKey(t, "rootserver", "web1", serverItem())
	for field, value := range map[string]any{
		"password":         "another-password",
		"ssh_key_ids":      []int64{9},
		"hostname":         "renamed.example.com",
		"client_reference": "ignored-the-argument-counts",
		"unknown_field":    true,
	} {
		item := serverItem()
		item[field] = value
		if mustKey(t, "rootserver", "web1", item) != base {
			t.Errorf("changing %s changed the key", field)
		}
	}
	item := serverItem()
	item["backups"] = nil
	withoutBackups := serverItem()
	delete(withoutBackups, "backups")
	if mustKey(t, "rootserver", "web1", item) != mustKey(t, "rootserver", "web1", withoutBackups) {
		t.Error("null and absent must give the same key")
	}

	dbase := mustKey(t, "domain", "example.de", domainItem())
	for field, value := range map[string]any{"contact_id": 99, "authcode": "secret-auth"} {
		item := domainItem()
		item[field] = value
		if mustKey(t, "domain", "example.de", item) != dbase {
			t.Errorf("domain: changing %s changed the key", field)
		}
	}
}
