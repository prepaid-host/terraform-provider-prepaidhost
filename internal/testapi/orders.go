// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Order endpoints, modelled on app/Http/Controllers/Api/OrderController.php
// and OrderPlacementService: POST /orders/quote, POST /orders, GET /orders/{uuid}.
func init() {
	register(func(s *Server) {
		s.Handle(http.MethodPost, "/orders/quote", handleQuote)
		s.Handle(http.MethodPost, "/orders", handlePlaceOrder)
		s.Handle(http.MethodGet, "/orders/{uuid}", handleGetOrder)
		s.Handle(http.MethodGet, "/account/balance", handleBalance)
	})
}

const maxOrderItems = 10

var resourceTypeOf = map[string]string{
	"rootserver": ResourceRootserver,
	"rootpacket": ResourceRootserver,
	"domain":     ResourceDomain,
}

var statusRank = map[string]int{"active": 0, "provisioning": 1, "failed": 2}

type orderBody struct {
	Items         []map[string]any `json:"items"`
	PromoCode     *string          `json:"promo_code"`
	ExpectedTotal *float64         `json:"expected_total"`
}

type pricedItem struct {
	item      map[string]any
	typ       string
	reference *string
	total     float64
	months    int
}

type pricing struct {
	items    []pricedItem
	subtotal float64
	discount float64
	total    float64
}

func validationFailed(fields map[string][]string) Response {
	return Fail(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Die Eingabe ist ungültig.", map[string]any{"fields": fields})
}

// parseOrder validates the body like QuoteOrderRequest (+ PlaceOrderRequest when placing).
func parseOrder(r *Request, placing bool) (*orderBody, *Response) {
	var body orderBody
	if err := r.Decode(&body); err != nil {
		resp := validationFailed(map[string][]string{"items": {"The items field is required."}})
		return nil, &resp
	}

	fields := map[string][]string{}
	if len(body.Items) == 0 || len(body.Items) > maxOrderItems {
		fields["items"] = []string{fmt.Sprintf("The items field must have between 1 and %d items.", maxOrderItems)}
	}
	for i, it := range body.Items {
		typ, _ := it["type"].(string)
		if _, ok := resourceTypeOf[typ]; !ok {
			fields[fmt.Sprintf("items.%d.type", i)] = []string{"The selected type is invalid."}
			continue
		}
		if typ != "domain" && it["template_id"] == nil {
			fields[fmt.Sprintf("items.%d.template_id", i)] = []string{"The template_id field is required."}
		}
		if ref, ok := it["client_reference"].(string); ok && len(ref) > 64 {
			fields[fmt.Sprintf("items.%d.client_reference", i)] = []string{"The client_reference may not be greater than 64 characters."}
		}
	}
	if body.PromoCode != nil && len(*body.PromoCode) > 50 {
		fields["promo_code"] = []string{"The promo_code may not be greater than 50 characters."}
	}
	if placing {
		if body.ExpectedTotal == nil || *body.ExpectedTotal < 0 {
			fields["expected_total"] = []string{"The expected_total field is required."}
		}
		if key := r.Header.Get("Idempotency-Key"); len(key) < 8 || len(key) > 255 {
			fields["idempotency_key"] = []string{"The Idempotency-Key field must be between 8 and 255 characters."}
		}
	}
	if len(fields) > 0 {
		resp := validationFailed(fields)
		return nil, &resp
	}
	return &body, nil
}

// invalidField is OrderingException::invalid — one field in "field", unlike the
// FormRequest validation which lists "fields".
func invalidField(field, message string) Response {
	return Fail(http.StatusUnprocessableEntity, "VALIDATION_FAILED", message, map[string]any{"field": field})
}

// domainName is sld + "." + tld, lower-cased like DomainConfigurator.
func domainName(it map[string]any) string {
	sld, _ := it["sld"].(string)
	tld, _ := it["tld"].(string)
	return strings.ToLower(sld) + "." + strings.ToLower(strings.TrimPrefix(tld, "."))
}

// reference is the item's client_reference, or the hostname for servers.
func reference(it map[string]any) *string {
	if ref, ok := it["client_reference"].(string); ok && ref != "" {
		return &ref
	}
	if it["type"] != "domain" {
		if host, ok := it["hostname"].(string); ok && host != "" {
			return &host
		}
	}
	return nil
}

func durationMonths(it map[string]any) int {
	if v, ok := it["duration_months"].(float64); ok && v >= 1 {
		return int(v)
	}
	if v, ok := it["duration_days"].(float64); ok && v >= 1 {
		return max(1, int(math.Round(v/30)))
	}
	return 1
}

func (st *State) price(body *orderBody) (*pricing, *Response) {
	p := &pricing{}
	seen := map[string]bool{}
	for i, it := range body.Items {
		typ, _ := it["type"].(string)
		if typ == "domain" {
			// DomainConfigurator: sld + tld are required; the API never takes a full "name".
			for _, key := range []string{"sld", "tld"} {
				if v, _ := it[key].(string); v == "" {
					resp := invalidField(fmt.Sprintf("items.%d.%s", i, key), "The "+key+" field is required.")
					return nil, &resp
				}
			}
			name := domainName(it)
			if st.UnavailableDomains[name] {
				resp := Fail(http.StatusUnprocessableEntity, "DOMAIN_UNAVAILABLE", fmt.Sprintf("Die Domain %s ist nicht registrierbar.", name),
					map[string]any{"domain": name})
				return nil, &resp
			}
		}
		ref := reference(it)
		if ref != nil {
			if seen[*ref] {
				resp := invalidField(fmt.Sprintf("items.%d.client_reference", i), "Duplicate client_reference "+*ref+".")
				return nil, &resp
			}
			seen[*ref] = true
		}
		total := round2(st.Prices[typ])
		p.items = append(p.items, pricedItem{item: it, typ: typ, reference: ref, total: total, months: durationMonths(it)})
		p.subtotal += total
	}
	p.subtotal = round2(p.subtotal)
	if body.PromoCode != nil {
		if d, ok := st.Promos[*body.PromoCode]; ok {
			p.discount = round2(math.Min(d, p.subtotal))
		}
	}
	p.total = round2(p.subtotal - p.discount)
	return p, nil
}

func handleQuote(st *State, r *Request) Response {
	body, errResp := parseOrder(r, false)
	if errResp != nil {
		return *errResp
	}
	p, errResp := st.price(body)
	if errResp != nil {
		return *errResp
	}

	items := make([]map[string]any, 0, len(p.items))
	for _, it := range p.items {
		var monthly any
		if it.typ != "domain" {
			monthly = round2(it.total / float64(it.months))
		}
		name, _ := it.item["hostname"].(string)
		if it.typ == "domain" {
			name = domainName(it.item)
		}
		items = append(items, map[string]any{
			"type":            it.typ,
			"name":            name,
			"price_monthly":   monthly,
			"duration_months": it.months,
			"total":           it.total,
		})
	}
	balance := round2(st.Balance)
	return OK(map[string]any{
		"items":      items,
		"subtotal":   p.subtotal,
		"discount":   p.discount,
		"total":      p.total,
		"balance":    balance,
		"affordable": balance >= p.total,
	})
}

// requestHash fingerprints the body like OrderPlacementService::requestHash:
// items with sorted keys (json.Marshal sorts map keys), expected_total with
// two decimals, promo_code trimmed with "" meaning absent.
func requestHash(body *orderBody) string {
	var promo *string
	if body.PromoCode != nil {
		if p := strings.TrimSpace(*body.PromoCode); p != "" {
			promo = &p
		}
	}
	b, _ := json.Marshal(map[string]any{
		"items":          body.Items,
		"expected_total": fmt.Sprintf("%.2f", *body.ExpectedTotal),
		"promo_code":     promo,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func handlePlaceOrder(st *State, r *Request) Response {
	body, errResp := parseOrder(r, true)
	if errResp != nil {
		return *errResp
	}

	key := r.Header.Get("Idempotency-Key")
	hash := requestHash(body)
	if entry, ok := st.Idempotency[key]; ok {
		switch {
		case entry.PendingAnswers > 0:
			entry.PendingAnswers--
			return Fail(http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", "Eine Anfrage mit diesem Idempotency-Key läuft noch.", nil)
		case entry.InProgress:
			return Fail(http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", "Eine Anfrage mit diesem Idempotency-Key läuft noch.", nil)
		case entry.RequestHash != hash:
			return Fail(http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Der Idempotency-Key wurde mit anderem Inhalt verwendet.", nil)
		default:
			return entry.Response.WithHeader("Idempotent-Replayed", "true")
		}
	}

	p, errResp := st.price(body)
	if errResp != nil {
		return *errResp
	}
	if round2(*body.ExpectedTotal) != p.total {
		return Fail(http.StatusConflict, "PRICE_CHANGED", "Der Preis hat sich geändert.",
			map[string]any{"expected": round2(*body.ExpectedTotal), "actual": p.total})
	}
	for _, it := range p.items {
		if it.reference == nil {
			continue
		}
		if typ, id, ok := st.referenceInUse(*it.reference); ok {
			return Fail(http.StatusConflict, "CLIENT_REFERENCE_IN_USE", fmt.Sprintf("Die Kennung %s ist bereits vergeben.", *it.reference),
				map[string]any{"resource_type": typ, "resource_id": id})
		}
	}
	if round2(st.Balance) < p.total {
		return Fail(http.StatusPaymentRequired, "INSUFFICIENT_BALANCE", "Das Guthaben reicht nicht aus.",
			map[string]any{"missing_amount": round2(p.total - st.Balance)})
	}

	st.nextOrder++
	order := &Order{
		ID:          fmt.Sprintf("00000000-0000-4000-8000-%012d", st.nextOrder),
		Total:       p.total,
		CreatedAt:   nowRFC3339(),
		pollsLeft:   st.ProvisionPolls,
		outcome:     st.ProvisionOutcome,
		requestHash: hash,
	}
	for _, it := range p.items {
		resType := resourceTypeOf[it.typ]
		attrs := copyMap(it.item)
		delete(attrs, "type")
		attrs["order_type"] = it.typ
		if it.reference != nil {
			attrs["client_reference"] = *it.reference
		}
		if it.typ == "domain" {
			attrs["name"] = domainName(it.item)
		}
		attrs["auto_renew"] = true
		attrs["created_at"] = nowRFC3339()
		attrs["expires_at"] = time.Now().AddDate(0, it.months, 0).UTC().Format(time.RFC3339)
		if fn := st.Provisioners[resType]; fn != nil {
			fn(it.item, attrs)
		}
		id := st.AddResource(resType, attrs)
		order.Items = append(order.Items, &OrderItem{Type: it.typ, ResourceType: resType, ResourceID: id, ClientReference: it.reference})
	}

	st.Balance = round2(st.Balance - p.total)
	st.Charges = append(st.Charges, Charge{OrderID: order.ID, Amount: p.total})
	st.Orders[order.ID] = order

	resp := Created(order.present(st, false))
	st.Idempotency[key] = &IdempotencyEntry{RequestHash: hash, Response: resp, PendingAnswers: st.SlowOrderAnswers}
	st.SlowOrderAnswers = 0
	return resp
}

func (st *State) referenceInUse(ref string) (string, int64, bool) {
	for _, typ := range []string{ResourceRootserver, ResourceDomain} {
		for _, id := range st.ResourceIDs(typ) {
			attrs := st.Resources[typ][id]
			if attrs["client_reference"] == ref && IsLive(attrs) {
				return typ, id, true
			}
		}
	}
	return "", 0, false
}

// present renders the order like OrderPlacementService::present. A poll
// advances the simulated provisioning by one step.
func (o *Order) present(st *State, poll bool) map[string]any {
	status := "provisioning"
	if o.pollsLeft <= 0 {
		status = o.outcome
	}
	if poll && o.pollsLeft > 0 {
		o.pollsLeft--
	}

	overall := "active"
	items := make([]map[string]any, 0, len(o.Items))
	for _, it := range o.Items {
		s := status
		if it.ForcedStatus != "" {
			s = it.ForcedStatus
		}
		if _, exists := st.Resource(it.ResourceType, it.ResourceID); !exists {
			// The API reports an item whose resource no longer exists as failed.
			s = "failed"
		}
		if statusRank[s] > statusRank[overall] {
			overall = s
		}
		var ref any
		if it.ClientReference != nil {
			ref = *it.ClientReference
		}
		items = append(items, map[string]any{
			"type":             it.Type,
			"resource_type":    it.ResourceType,
			"resource_id":      it.ResourceID,
			"client_reference": ref,
			"status":           s,
		})
	}
	return map[string]any{
		"id":         o.ID,
		"status":     overall,
		"total":      o.Total,
		"created_at": o.CreatedAt,
		"items":      items,
	}
}

func handleGetOrder(st *State, r *Request) Response {
	o, ok := st.Orders[r.Param("uuid")]
	if !ok {
		return Fail(http.StatusNotFound, "ORDER_NOT_FOUND", "Bestellung nicht gefunden.", nil)
	}
	return OK(o.present(st, true))
}

func handleBalance(st *State, _ *Request) Response {
	return OK(map[string]any{"balance": round2(st.Balance), "currency": "EUR"})
}
