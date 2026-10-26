// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

// The order engine every purchasing resource (vServer, domain) uses for Create:
//
//	POST /orders/quote → max_price check → POST /orders (expected_total +
//	Idempotency-Key) → GET /orders/{uuid} until the item is active.
//
// Double purchases are prevented twice over:
//   - The Idempotency-Key is derived from the order itself (idempotencyKey), so
//     an apply repeated after an abort — even in a new process — sends the same
//     key and the API replays the paid order instead of charging again.
//   - Every order carries a client_reference; while a resource with it lives,
//     the API refuses a new one (CLIENT_REFERENCE_IN_USE) and names it, and the
//     engine takes it over.

const (
	defaultCreateTimeout = 30 * time.Minute
	// maxInProgressWait caps the wait between two attempts while the API is
	// still processing an earlier request with the same key.
	maxInProgressWait  = 30 * time.Second
	maxClientReference = 64
)

// orderResourceTypes maps an order item type to the resource type the API
// creates for it (OrderItem.resource_type).
var orderResourceTypes = map[string]string{
	"rootserver": "rootserver",
	"rootpacket": "rootserver",
	"domain":     "domain",
}

// purchaseFields are the item fields that decide what is bought and what it
// costs — the material of the Idempotency-Key. It is an allowlist on purpose:
//
//   - Secrets stay out (password, authcode): the key is a plain SHA-256 sent to
//     the API in a header, and a hash of a password plus guessable fields could
//     be brute-forced. (The API itself keys its request fingerprint with an
//     HMAC for exactly this reason.)
//   - Fields that change neither product nor price stay out (ssh_key_ids,
//     hostname, contact_id): changing them must not turn a repeated apply into
//     a new purchase.
//   - promo_code is in: it changes the price.
//
// A field missing here can never cause a double purchase: the same key with a
// different body is answered with IDEMPOTENCY_CONFLICT, which is an error.
var purchaseFields = map[string][]string{
	"rootserver": {"cores", "memory", "disk", "addresses", "backups", "traffic", "duration_days", "duration_months", "template_id", "promo_code"},
	"rootpacket": {"packet_id", "duration_months", "duration_days", "template_id", "promo_code"},
	"domain":     {"sld", "tld", "name", "duration_months", "action", "promo_code"},
}

// orderResource buys one item and waits until it is provisioned.
//
// item holds "type" (rootserver|rootpacket|domain) and the product fields of
// that type; clientReference (required, at most 64 characters) is set as the
// item's client_reference. maxPrice, if not nil, is the highest total the
// caller accepts — a higher quote is an error before anything is ordered.
// timeout bounds the whole call (default 30 minutes when <= 0).
//
// adopted is true when the resource was not bought by this call: either the
// API named a live resource with the same client reference
// (CLIENT_REFERENCE_IN_USE), or it replayed an order placed earlier with the
// same Idempotency-Key (an aborted apply, or apply after destroy). The caller
// then switches auto-renew on again and warns that an existing resource was
// adopted. A replay can also be this call's own order whose response was lost
// and retried; adopting it is harmless.
//
// On error resourceID is 0, except when the order was paid but the item
// failed or did not become active in time: then it names the resource. The
// caller must not save state in that case — the next apply sends the same key
// and gets the same order back.
func orderResource(ctx context.Context, c *client.Client, item map[string]any, clientReference string, maxPrice *float64, timeout time.Duration) (resourceID int64, adopted bool, diags diag.Diagnostics) {
	itemType, _ := item["type"].(string)
	wantResourceType, ok := orderResourceTypes[itemType]
	if !ok {
		diags.AddError("Unsupported order type", fmt.Sprintf("Cannot order an item of type %q (provider bug).", itemType))
		return 0, false, diags
	}
	if clientReference == "" || len(clientReference) > maxClientReference {
		diags.AddError("Invalid client reference",
			fmt.Sprintf("Every order needs a client reference of 1 to %d characters, got %q (provider bug).", maxClientReference, clientReference))
		return 0, false, diags
	}
	key, err := idempotencyKey(itemType, clientReference, item)
	if err != nil {
		diags.AddError("Cannot build the order", err.Error())
		return 0, false, diags
	}
	if timeout <= 0 {
		timeout = defaultCreateTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := client.OrderRequest{Items: []map[string]any{orderItem(item, clientReference)}}

	// 1. Quote: the binding price, nothing is charged.
	quote, err := c.Quote(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			diags.AddError("Order timed out", fmt.Sprintf("No price quote within %s. Nothing was ordered or charged; run apply again.\n\n%s",
				timeout, describeError(err)))
			return 0, false, diags
		}
		addOrderError(&diags, "Price quote failed", err, nil)
		return 0, false, diags
	}

	// 2. Cost protection.
	if maxPrice != nil && cents(quote.Total) > cents(*maxPrice) {
		diags.AddError("Price above max_price",
			fmt.Sprintf("The order costs %.2f EUR, max_price allows %.2f EUR. Nothing was ordered or charged. "+
				"Raise max_price or change the configuration.", quote.Total, *maxPrice))
		return 0, false, diags
	}

	// 3. Order. While the API still processes an earlier request with this key
	//    (IDEMPOTENCY_IN_PROGRESS — e.g. a slow request that outlived the HTTP
	//    timeout and was retried), wait and retry with the SAME key until the
	//    order is replayed, an error is definitive, or the timeout ends.
	order, err := placeOrder(ctx, c, req, quote.Total, key)
	if err != nil {
		if client.IsCode(err, client.CodeClientReferenceInUse) {
			return adoptReference(err, wantResourceType, clientReference)
		}
		switch {
		case client.IsCode(err, client.CodeIdempotencyInProgress):
			diags.AddError("Order timed out",
				fmt.Sprintf("An earlier request for this order (same Idempotency-Key) was still being processed by the API after %s. "+
					"Run apply again — it will not be bought twice: the next apply uses the same key and gets this order.\n\n%s",
					timeout, describeError(err)))
		case ctx.Err() != nil || isTransportError(err):
			diags.AddError("Order outcome unknown",
				fmt.Sprintf("The order request did not complete (timeout %s); it may or may not have been placed. "+
					"Run apply again — it will not be bought twice: the next apply uses the same Idempotency-Key and gets the order if it was placed.\n\n%s",
					timeout, describeError(err)))
		default:
			addOrderError(&diags, "Order failed", err, quote)
		}
		return 0, false, diags
	}

	if len(order.Items) != 1 {
		diags.AddError("Unexpected order response", fmt.Sprintf("Order %s has %d items, expected 1.", order.ID, len(order.Items)))
		return 0, false, diags
	}
	placed := order.Items[0]
	if placed.ResourceType != wantResourceType || placed.ResourceID == 0 {
		diags.AddError("Unexpected order response",
			fmt.Sprintf("Order %s created %s %d, expected a %s.", order.ID, placed.ResourceType, placed.ResourceID, wantResourceType))
		return 0, false, diags
	}
	resourceID, adopted = placed.ResourceID, order.Replayed

	// 4. Wait for provisioning.
	if placed.Status != client.StatusActive {
		_, err := c.WaitForItem(ctx, order.ID, 0, 0)
		switch {
		case err == nil:
		case errors.Is(err, client.ErrItemFailed):
			diags.AddError("Provisioning failed",
				fmt.Sprintf("Order %s was paid, but provisioning %s %d failed. Contact support with the order id; "+
					"running apply again does not buy it again (same Idempotency-Key) but will report the same failure.",
					order.ID, placed.ResourceType, placed.ResourceID))
			return resourceID, adopted, diags
		case ctx.Err() != nil:
			diags.AddError("Order timed out",
				fmt.Sprintf("Order %s is already paid, but %s %d was not active after %s. "+
					"Run apply again — it will not be bought twice: the next apply uses the same Idempotency-Key and takes this order over.\n\n%s",
					order.ID, placed.ResourceType, placed.ResourceID, timeout, err))
			return resourceID, adopted, diags
		default:
			diags.AddError("Order status unknown",
				fmt.Sprintf("Order %s is already paid, but its status could not be read. "+
					"Run apply again — it will not be bought twice: the next apply uses the same Idempotency-Key and takes this order over.\n\n%s",
					order.ID, describeError(err)))
			return resourceID, adopted, diags
		}
	}

	return resourceID, adopted, diags
}

// placeOrder sends POST /orders and waits out IDEMPOTENCY_IN_PROGRESS with
// backoff (poll interval, doubling, capped), always with the same key. It
// returns the last error when the context ends.
func placeOrder(ctx context.Context, c *client.Client, req client.OrderRequest, total float64, key string) (*client.Order, error) {
	wait := c.PollInterval()
	for {
		order, err := c.PlaceOrder(ctx, req, total, key)
		if !client.IsCode(err, client.CodeIdempotencyInProgress) {
			return order, err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
		wait = min(wait*2, max(maxInProgressWait, c.PollInterval()))
	}
}

func adoptReference(err error, wantResourceType, clientReference string) (int64, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	apiErr, _ := client.AsAPIError(err)
	id, ok := apiErr.Int64("resource_id")
	resourceType, _ := apiErr.String("resource_type")
	if !ok || id <= 0 {
		diags.AddError("Client reference in use",
			fmt.Sprintf("The client reference %q is used by a live resource, but the API did not name it.\n\n%s", clientReference, describeError(err)))
		return 0, false, diags
	}
	if resourceType != wantResourceType {
		diags.AddError("Client reference in use",
			fmt.Sprintf("The client reference %q is used by %s %d, which cannot be managed as a %s. Nothing was charged. "+
				"Use another name.\n\n%s", clientReference, resourceType, id, wantResourceType, describeError(err)))
		return 0, false, diags
	}
	return id, true, diags
}

// addOrderError adds an API error with a hint on what to do about it.
// quote, when known, adds the order total to balance errors.
func addOrderError(diags *diag.Diagnostics, summary string, err error, quote *client.Quote) {
	detail := describeError(err)
	if hint := orderErrorHint(err, quote); hint != "" {
		detail = hint + "\n\n" + detail
	}
	diags.AddError(summary, detail)
}

func orderErrorHint(err error, quote *client.Quote) string {
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		return ""
	}
	switch apiErr.Code {
	case client.CodeInsufficientBalance:
		missing, _ := apiErr.Float("missing_amount")
		hint := fmt.Sprintf("The balance is %.2f EUR short", missing)
		if quote != nil {
			hint += fmt.Sprintf(" (order total %.2f EUR)", quote.Total)
		}
		return hint + ". Nothing was charged. Top up the balance in the customer panel and run apply again."
	case client.CodePriceChanged:
		expected, _ := apiErr.Float("expected")
		actual, _ := apiErr.Float("actual")
		return fmt.Sprintf("The price changed between quote and order: quoted %.2f EUR, now %.2f EUR. Nothing was charged. "+
			"Run apply again to order at the current price (max_price caps it).", expected, actual)
	case client.CodePromoCodeUnavailable:
		return "The promo code was used up while the order was placed. Nothing was charged. Remove promo_code or use another code and run apply again."
	case client.CodeIdempotencyConflict:
		return "An order with the same product configuration was already placed and paid, but with other details " +
			"(for example another password, SSH keys, contact or a changed price). Nothing was charged now. " +
			"Restore the configuration of that order, or take the existing resource over with `terraform import`."
	case client.CodeValidationFailed:
		if field, ok := apiErr.String("field"); ok {
			return fmt.Sprintf("The API rejected the value of %q: %s", orderField(field), apiErr.Message)
		}
		return "The API rejected the order; see the fields below."
	case client.CodeDomainUnavailable:
		domain, _ := apiErr.String("domain")
		return fmt.Sprintf("The domain %s cannot be registered (taken or not allowed). Choose another name, or use action = \"transfer\" with the auth code if you own it.", domain)
	case client.CodeOrderFailed:
		return "The order failed and was rolled back; nothing was charged. Run apply again."
	case client.CodeInsufficientScope:
		scope, _ := apiErr.String("required_scope")
		return fmt.Sprintf("The API token lacks the scope %q. Create a token with this scope in the customer panel and accept the terms.", scope)
	case "TERMS_NOT_ACCEPTED":
		return "The API token has no consent to the current ordering terms. Create a new API token in the customer panel and accept the terms when creating it."
	case "ORDER_REQUIRES_BEARER_TOKEN":
		return "Ordering needs a personal API token (Bearer token) from the customer panel; the legacy api_token cannot place orders."
	case "ACCOUNT_NOT_ACTIVE":
		return "The customer account is not active or is suspended, so nothing can be ordered. Check the customer panel or contact support."
	}
	return ""
}

// orderField turns "items.0.cores" into "cores".
func orderField(field string) string {
	if rest, ok := strings.CutPrefix(field, "items."); ok {
		if i := strings.IndexByte(rest, '.'); i >= 0 {
			return rest[i+1:]
		}
	}
	return field
}

func isTransportError(err error) bool {
	_, isAPI := client.AsAPIError(err)
	return !isAPI
}

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// orderItem is the item as sent: nil fields dropped (absent and null are the
// same to the API and to the key) and the client reference set.
func orderItem(item map[string]any, clientReference string) map[string]any {
	out := make(map[string]any, len(item)+1)
	for k, v := range item {
		if v != nil {
			out[k] = v
		}
	}
	out["client_reference"] = clientReference
	return out
}

// idempotencyKey is "tf-" + hex(sha256(type + "\x00" + clientReference + "\x00"
// + canonical JSON of the purchase fields)). It depends only on the
// configuration, so every process computes the same key for the same order.
// Canonical JSON: object keys sorted, no whitespace, nil/absent fields
// omitted, integral numbers without fraction (2, 2.0 and "2" as json.Number
// are all 2). The key material holds no secrets (see purchaseFields) and is
// never logged.
func idempotencyKey(itemType, clientReference string, item map[string]any) (string, error) {
	fields, ok := purchaseFields[itemType]
	if !ok {
		return "", fmt.Errorf("unsupported order type %q", itemType)
	}
	relevant := map[string]any{}
	for _, f := range fields {
		if v, ok := item[f]; ok && v != nil {
			relevant[f] = v
		}
	}
	canonical, err := canonicalJSON(relevant)
	if err != nil {
		return "", fmt.Errorf("encoding the order for its idempotency key: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(itemType))
	h.Write([]byte{0})
	h.Write([]byte(clientReference))
	h.Write([]byte{0})
	h.Write(canonical)
	return "tf-" + hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := writeCanonical(&b, generic); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		b.WriteString(canonicalNumber(x))
	case string:
		s, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(s)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			if x[k] != nil {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			ks, _ := json.Marshal(k)
			b.Write(ks)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unexpected JSON value %T", v)
	}
	return nil
}

// canonicalNumber writes integral values as integers and everything else in
// the shortest form that round-trips.
func canonicalNumber(n json.Number) string {
	if i, err := n.Int64(); err == nil {
		return strconv.FormatInt(i, 10)
	}
	f, err := n.Float64()
	if err != nil {
		return n.String()
	}
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
