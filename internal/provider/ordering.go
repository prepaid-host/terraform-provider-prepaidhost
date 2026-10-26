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
	// maxKeyChain bounds the follow-up keys used to buy again after the terms
	// of earlier orders under this configuration ran out. At the end of the
	// chain the engine stops with an error rather than trying one more key:
	// whether that key would replay, meet a live server (take-over) or buy
	// can only be learnt by sending it, so "one more" would just be a longer
	// chain. Eleven purchases of one configuration under one client
	// reference are far beyond normal use; the error names the way out
	// (another hostname/name).
	maxKeyChain = 10
	// replayTolerance is how much younger than this call a replayed order may
	// look and still count as placed before it (see placedBefore).
	replayTolerance = 2 * time.Minute
)

// rootserverMonths maps a rootserver's duration_days to the months of its
// rental, like RootserverFulfiller.
var rootserverMonths = map[int]int{30: 1, 90: 3, 180: 6, 365: 12}

// orderResourceTypes maps an order item type to the resource type the API
// creates for it (OrderItem.resource_type).
var orderResourceTypes = map[string]string{
	"rootserver": "rootserver",
	"rootpacket": "rootserver",
	"domain":     "domain",
}

// purchaseFields are the item fields that decide what is bought and what it
// costs — the material of the Idempotency-Key. It is an allowlist of fields
// the API actually evaluates (OrderPlacementService::INPUT_KEYS), on purpose:
//
//   - Secrets stay out (password, authcode): the key is a plain SHA-256 sent to
//     the API in a header, and a hash of a password plus guessable fields could
//     be brute-forced. (The API itself keys its request fingerprint with an
//     HMAC for exactly this reason.)
//   - Fields that change neither product nor price stay out (ssh_key_ids,
//     hostname, contact_id): changing them must not turn a repeated apply into
//     a new purchase.
//   - rootpacket's item promo_code (a packet promotion keyword) is in: it
//     changes the price. The order-level promo code is added separately
//     (orderOptions.PromoCode).
//
// A field missing here can never cause a double purchase: the same key with a
// different body is answered with IDEMPOTENCY_CONFLICT.
var purchaseFields = map[string][]string{
	"rootserver": {"cores", "memory", "disk", "addresses", "backups", "traffic", "duration_days", "template_id"},
	"rootpacket": {"packet_id", "duration_months", "template_id", "promo_code"},
	"domain":     {"sld", "tld", "duration_months", "action"},
}

// orderOptions are the optional order-level settings.
type orderOptions struct {
	// PromoCode is the order's promo code (trimmed; "" = none). It changes the
	// price and is therefore part of the Idempotency-Key.
	PromoCode string
}

// orderOutcome says where the resource returned by orderResource comes from.
type orderOutcome int

const (
	// orderNew: bought by this call. A replay of this call's own order whose
	// response was lost usually counts as orderReplayed instead (see
	// placedBefore) — harmless for the caller.
	orderNew orderOutcome = iota
	// orderReplayed: an order placed before this call with the same
	// configuration (an aborted apply, or apply after destroy) was replayed;
	// nothing was charged now. Its resource may have auto-renew switched off.
	orderReplayed
	// orderReferenceInUse: a live resource with the same client reference
	// already existed (CLIENT_REFERENCE_IN_USE) and is taken over; nothing
	// was charged.
	orderReferenceInUse
)

func (o orderOutcome) String() string {
	switch o {
	case orderReplayed:
		return "replayed"
	case orderReferenceInUse:
		return "reference in use"
	}
	return "new"
}

// orderResource buys one item and waits until it is provisioned.
//
// item holds "type" (rootserver|rootpacket|domain) and the product fields of
// that type; clientReference (required, at most 64 characters) is set as the
// item's client_reference. maxPrice, if not nil, is the highest total the
// caller accepts — a higher quote is an error before anything is ordered.
// timeout bounds the whole call (default 30 minutes when <= 0).
//
// outcome tells the caller how to go on: for orderReplayed and
// orderReferenceInUse the resource existed before this call, so the caller
// switches auto-renew on again (destroy switched it off) and warns that an
// existing resource was taken over; orderNew needs neither.
//
// Keys: the first attempt uses idempotencyKey. Two cases move on to a
// deterministic follow-up key, so a repeated apply walks the same path:
//   - The key replays an order whose term is over (created_at + duration in
//     the past): chainKey(key, order id), up to maxKeyChain links.
//   - The key answers IDEMPOTENCY_CONFLICT (same configuration ordered with
//     other details, e.g. a changed password or price): conflictKey, once.
//
// Neither buys twice: the API refuses a new order while a live resource
// carries the client reference (checked under the user lock, before the
// balance) and names it instead — the engine then takes it over. A replay
// within the term is never bought again, even when its item failed.
//
// On error resourceID is 0, except when the order was paid but the item
// failed or did not become active in time: then it names the resource. The
// caller must not save state in that case — the next apply walks the same
// keys and gets the same order back.
func orderResource(ctx context.Context, c *client.Client, item map[string]any, clientReference string, maxPrice *float64, timeout time.Duration, opts orderOptions) (resourceID int64, outcome orderOutcome, diags diag.Diagnostics) {
	start := time.Now()
	itemType, _ := item["type"].(string)
	wantResourceType, ok := orderResourceTypes[itemType]
	if !ok {
		diags.AddError("Unsupported order type", fmt.Sprintf("Cannot order an item of type %q (provider bug).", itemType))
		return 0, orderNew, diags
	}
	if clientReference == "" || len(clientReference) > maxClientReference {
		diags.AddError("Invalid client reference",
			fmt.Sprintf("Every order needs a client reference of 1 to %d characters, got %q (provider bug).", maxClientReference, clientReference))
		return 0, orderNew, diags
	}
	promoCode := strings.TrimSpace(opts.PromoCode)
	key, err := idempotencyKey(itemType, clientReference, item, promoCode)
	if err != nil {
		diags.AddError("Cannot build the order", err.Error())
		return 0, orderNew, diags
	}
	if timeout <= 0 {
		timeout = defaultCreateTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := client.OrderRequest{Items: []map[string]any{orderItem(item, clientReference)}, PromoCode: promoCode}

	// 1. Quote: the binding price, nothing is charged.
	quote, err := c.Quote(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			diags.AddError("Order timed out", fmt.Sprintf("No price quote within %s. Nothing was ordered or charged; run apply again.\n\n%s",
				timeout, describeError(err)))
			return 0, orderNew, diags
		}
		addOrderError(&diags, "Price quote failed", err, nil)
		return 0, orderNew, diags
	}

	// 2. Cost protection.
	if maxPrice != nil && cents(quote.Total) > cents(*maxPrice) {
		diags.AddError("Price above max_price",
			fmt.Sprintf("The order costs %.2f EUR, max_price allows %.2f EUR. Nothing was ordered or charged. "+
				"Raise max_price or change the configuration.", quote.Total, *maxPrice))
		return 0, orderNew, diags
	}

	// 3. Order, following the key rules above.
	var order *client.Order
	conflictRetried := false
	for links := 0; ; {
		order, err = placeOrder(ctx, c, req, quote.Total, key)
		if err != nil {
			if client.IsCode(err, client.CodeClientReferenceInUse) {
				return adoptReference(err, wantResourceType, clientReference)
			}
			if client.IsCode(err, client.CodeIdempotencyConflict) && !conflictRetried {
				conflictRetried = true
				key = conflictKey(key, quote.Total)
				continue
			}
			addPlaceOrderError(ctx, &diags, err, timeout, quote, conflictRetried)
			return 0, orderNew, diags
		}
		if !order.Replayed || !termOver(order, item, time.Now()) {
			break
		}
		if links >= maxKeyChain {
			diags.AddError("Too many earlier orders",
				fmt.Sprintf("This configuration was ordered more than %d times before and every term is over; the provider follows the chain no further. "+
					"Nothing was charged now. Use another client reference (hostname or name).", maxKeyChain))
			return 0, orderNew, diags
		}
		// The order under this key ran out: buy again under the next link.
		key = chainKey(key, order.ID)
		links++
	}
	if conflictRetried && !order.Replayed {
		diags.AddWarning("Ordered under a follow-up key",
			"An earlier order with the same configuration but other details (for example another password or price) exists, "+
				"and no live resource carries this client reference any more, so the item was bought again: "+
				"the earlier resource ran out or was deleted.")
	}

	if len(order.Items) != 1 {
		diags.AddError("Unexpected order response", fmt.Sprintf("Order %s has %d items, expected 1.", order.ID, len(order.Items)))
		return 0, orderNew, diags
	}
	placed := order.Items[0]
	if placed.ResourceType != wantResourceType || placed.ResourceID == 0 {
		diags.AddError("Unexpected order response",
			fmt.Sprintf("Order %s created %s %d, expected a %s.", order.ID, placed.ResourceType, placed.ResourceID, wantResourceType))
		return 0, orderNew, diags
	}
	resourceID = placed.ResourceID
	if order.Replayed && placedBefore(order, start) {
		outcome = orderReplayed
	}

	// 4. Wait for provisioning.
	if placed.Status != client.StatusActive {
		_, err := c.WaitForItem(ctx, order.ID, 0, 0)
		switch {
		case err == nil:
		case errors.Is(err, client.ErrItemFailed):
			diags.AddError("Provisioning failed",
				fmt.Sprintf("Order %s was paid, but provisioning %s %d failed. Contact support with the order id. "+
					"Running apply again does not buy it again within its term (same Idempotency-Key) and reports this failure again.",
					order.ID, placed.ResourceType, placed.ResourceID))
			return resourceID, outcome, diags
		case ctx.Err() != nil:
			diags.AddError("Order timed out",
				fmt.Sprintf("Order %s is already paid, but %s %d was not active after %s. "+
					"Run apply again — it will not be bought twice: the next apply uses the same Idempotency-Key and takes this order over.\n\n%s",
					order.ID, placed.ResourceType, placed.ResourceID, timeout, err))
			return resourceID, outcome, diags
		default:
			diags.AddError("Order status unknown",
				fmt.Sprintf("Order %s is already paid, but its status could not be read. "+
					"Run apply again — it will not be bought twice: the next apply uses the same Idempotency-Key and takes this order over.\n\n%s",
					order.ID, describeError(err)))
			return resourceID, outcome, diags
		}
	}

	return resourceID, outcome, diags
}

func addPlaceOrderError(ctx context.Context, diags *diag.Diagnostics, err error, timeout time.Duration, quote *client.Quote, afterConflict bool) {
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
	case afterConflict && client.IsCode(err, client.CodeIdempotencyConflict):
		diags.AddError("Order conflicts with earlier orders",
			"An order with the same product configuration was already placed with other details (for example another password, SSH keys, "+
				"contact or price), and so was the follow-up order the provider tried instead. Nothing was charged now. "+
				"Restore the configuration of the earlier order, or take the existing resource over with `terraform import`.\n\n"+
				"About the follow-up order: the API refuses it while a live resource carries this client reference (the provider then "+
				"takes that resource over), so it only buys when no live resource with this reference exists — that is, when the "+
				"earlier resource ran out or was deleted, even before the end of its term.\n\n"+describeError(err))
	default:
		addOrderError(diags, "Order failed", err, quote)
	}
}

// termOver reports whether the term of a replayed order has ended:
// created_at + the item's duration in calendar months lies before now. The
// months are what the API rents: rootpacket and domain duration_months
// (default 1); rootserver duration_days mapped like RootserverFulfiller
// (30→1, 90→3, 180→6, 365→12, otherwise days/30 rounded, at least 1; default
// 1), added like Carbon::addMonths. An unreadable created_at counts as "not
// over": the engine then never buys again.
func termOver(order *client.Order, item map[string]any, now time.Time) bool {
	created, err := time.Parse(time.RFC3339, order.CreatedAt)
	if err != nil {
		return false
	}
	months := 1
	if itemType, _ := item["type"].(string); itemType == "rootserver" {
		if days, ok := number(item["duration_days"]); ok && days >= 1 {
			if m, known := rootserverMonths[int(days)]; known && float64(int(days)) == days {
				months = m
			} else {
				months = max(1, int(math.Round(days/30)))
			}
		}
	} else if m, ok := number(item["duration_months"]); ok && m >= 1 {
		months = int(m)
	}
	return created.AddDate(0, months, 0).Before(now)
}

// placedBefore reports whether a replayed order was placed before this call
// started, i.e. the resource existed before (an aborted apply, or apply after
// destroy) rather than this call's own order whose response was lost.
//
// created_at comes from the server clock, start from the local one. The
// comparison leans towards "before" by replayTolerance, because the two
// mistakes are not equal: a wrong "before" only switches auto-renew on once
// more (it already is on) and shows the neutral take-over warning; a wrong
// "not before" would leave auto-renew off after destroy + apply when the
// server clock runs ahead. The price: this call's own order replayed after a
// lost response usually counts as "before" too. An unreadable created_at
// counts as before as well.
func placedBefore(order *client.Order, start time.Time) bool {
	created, err := time.Parse(time.RFC3339, order.CreatedAt)
	if err != nil {
		return true
	}
	return created.Before(start.Add(replayTolerance))
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// chainKey is the key for buying again after the order under prev ran out:
// "tf-" + hex(sha256(prev + "\x00" + order id)).
func chainKey(prev, orderID string) string {
	sum := sha256.Sum256([]byte(prev + "\x00" + orderID))
	return "tf-" + hex.EncodeToString(sum[:])
}

// conflictKey is the single follow-up key after IDEMPOTENCY_CONFLICT:
// "tf-" + hex(sha256(key + "\x00conflict\x00" + total in cents)). No password
// material; a price change leads to its own key.
func conflictKey(key string, total float64) string {
	sum := sha256.Sum256([]byte(key + "\x00conflict\x00" + strconv.FormatInt(cents(total), 10)))
	return "tf-" + hex.EncodeToString(sum[:])
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

func adoptReference(err error, wantResourceType, clientReference string) (int64, orderOutcome, diag.Diagnostics) {
	var diags diag.Diagnostics
	apiErr, _ := client.AsAPIError(err)
	id, ok := apiErr.Int64("resource_id")
	resourceType, _ := apiErr.String("resource_type")
	if !ok || id <= 0 {
		diags.AddError("Client reference in use",
			fmt.Sprintf("The client reference %q is used by a live resource, but the API did not name it.\n\n%s", clientReference, describeError(err)))
		return 0, orderNew, diags
	}
	if resourceType != wantResourceType {
		diags.AddError("Client reference in use",
			fmt.Sprintf("The client reference %q is used by %s %d, which cannot be managed as a %s. Nothing was charged. "+
				"Use another name.\n\n%s", clientReference, resourceType, id, wantResourceType, describeError(err)))
		return 0, orderNew, diags
	}
	return id, orderReferenceInUse, diags
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
		return fmt.Sprintf("The domain %s cannot be registered (taken or not allowed). Nothing was charged. If it is already in "+
			"this account (registered in the customer panel), bring it in with `terraform import` instead. If you own it at another "+
			"registrar: transfers are not available through the API yet, order the transfer in the customer panel. Otherwise "+
			"choose another name.", domain)
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
// + canonical JSON of the purchase fields [+ "\x00promo_code\x00" + order
// promo code, when set])). It depends only on the
// configuration, so every process computes the same key for the same order.
// Canonical JSON: object keys sorted, no whitespace, nil/absent fields
// omitted, integral numbers without fraction (2, 2.0 and "2" as json.Number
// are all 2). The key material holds no secrets (see purchaseFields) and is
// never logged.
func idempotencyKey(itemType, clientReference string, item map[string]any, promoCode string) (string, error) {
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
	if promoCode != "" {
		h.Write([]byte("\x00promo_code\x00"))
		h.Write([]byte(promoCode))
	}
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
