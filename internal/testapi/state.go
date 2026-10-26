// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"sort"
	"time"
)

// Resource types the order endpoints create.
const (
	ResourceRootserver = "rootserver"
	ResourceDomain     = "domain"
)

// Default prices (item total) per order item type.
var defaultPrices = map[string]float64{
	"rootserver": 9.99,
	"rootpacket": 9.99,
	"domain":     12.00,
}

// Charge is one successful, charged POST /orders.
type Charge struct {
	OrderID string
	Amount  float64
}

// State is the mock's in-memory data. Handlers receive it with the lock held;
// tests reach it via Server.With or the helper methods.
type State struct {
	Balance float64
	// Prices is the total per order item type (rootserver, rootpacket, domain).
	Prices map[string]float64
	// Promos maps a promo code to its discount amount; unknown codes are ignored (like the API).
	Promos             map[string]float64
	UnavailableDomains map[string]bool

	// Resources holds every resource by type ("rootserver", "domain", "ssh_key",
	// "domain_contact", ...) and id. Attributes are free-form; each endpoint
	// file decides how it presents them.
	Resources map[string]map[int64]map[string]any

	// Catalogs overrides GET /catalog/{type} per type (see Server.SetCatalog).
	Catalogs map[string]map[string]any

	Orders      map[string]*Order
	Idempotency map[string]*IdempotencyEntry
	Charges     []Charge

	// ProvisionPolls is how many GET /orders/{uuid} answer "provisioning"
	// before the items reach ProvisionOutcome (default 1 and "active").
	ProvisionPolls   int
	ProvisionOutcome string
	// Provisioners can enrich a resource created by an order (e.g. IPs for a
	// rootserver). Key is the resource type.
	Provisioners map[string]func(item, attrs map[string]any)

	nextID    int64
	nextOrder int
}

// IdempotencyEntry is a used Idempotency-Key.
type IdempotencyEntry struct {
	RequestHash string
	InProgress  bool
	Response    Response
}

// Order is a placed order.
type Order struct {
	ID          string
	Total       float64
	CreatedAt   string
	Items       []*OrderItem
	pollsLeft   int
	outcome     string
	requestHash string
}

// OrderItem is one position of an Order.
type OrderItem struct {
	Type            string
	ResourceType    string
	ResourceID      int64
	ClientReference *string
	// ForcedStatus overrides the simulated provisioning status when set.
	ForcedStatus string
}

func newState() *State {
	prices := map[string]float64{}
	for k, v := range defaultPrices {
		prices[k] = v
	}
	return &State{
		Prices:             prices,
		Promos:             map[string]float64{},
		UnavailableDomains: map[string]bool{},
		Resources:          map[string]map[int64]map[string]any{},
		Orders:             map[string]*Order{},
		Idempotency:        map[string]*IdempotencyEntry{},
		ProvisionPolls:     1,
		ProvisionOutcome:   "active",
		Provisioners:       map[string]func(item, attrs map[string]any){},
		nextID:             1000,
	}
}

// NextID returns a fresh id, unique across all resource types.
func (st *State) NextID() int64 {
	st.nextID++
	return st.nextID
}

// AddResource stores attrs (copied) under a new id and sets attrs["id"].
func (st *State) AddResource(typ string, attrs map[string]any) int64 {
	id := st.NextID()
	st.PutResource(typ, id, attrs)
	return id
}

// PutResource stores attrs (copied) under id and sets attrs["id"].
func (st *State) PutResource(typ string, id int64, attrs map[string]any) {
	if st.Resources[typ] == nil {
		st.Resources[typ] = map[int64]map[string]any{}
	}
	cp := copyMap(attrs)
	cp["id"] = id
	st.Resources[typ][id] = cp
}

// Resource returns the stored attributes (not a copy — handlers may change them).
func (st *State) Resource(typ string, id int64) (map[string]any, bool) {
	r, ok := st.Resources[typ][id]
	return r, ok
}

// ResourceIDs returns the ids of a type in ascending order.
func (st *State) ResourceIDs(typ string) []int64 {
	ids := make([]int64, 0, len(st.Resources[typ]))
	for id := range st.Resources[typ] {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// IsLive reports whether a resource still runs: expires_at (RFC 3339) lies in
// the future. Like the API, a resource without expiry counts as not running.
func IsLive(attrs map[string]any) bool {
	s, ok := attrs["expires_at"].(string)
	if !ok {
		return false
	}
	t, err := time.Parse(time.RFC3339, s)
	return err == nil && t.After(time.Now())
}

func copyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// ---- Server helpers (lock the state) ----

// SetBalance sets the account balance.
func (s *Server) SetBalance(v float64) { s.With(func(st *State) { st.Balance = v }) }

// Balance returns the account balance.
func (s *Server) Balance() float64 {
	var v float64
	s.With(func(st *State) { v = st.Balance })
	return v
}

// SetPrice sets the total the mock charges for one item of itemType.
func (s *Server) SetPrice(itemType string, total float64) {
	s.With(func(st *State) { st.Prices[itemType] = total })
}

// SetPromo makes code a valid promo code worth discount.
func (s *Server) SetPromo(code string, discount float64) {
	s.With(func(st *State) { st.Promos[code] = discount })
}

// SetDomainUnavailable makes quote/order of this domain fail with DOMAIN_UNAVAILABLE.
func (s *Server) SetDomainUnavailable(name string) {
	s.With(func(st *State) { st.UnavailableDomains[name] = true })
}

// SetProvisioningPolls sets how many order polls answer "provisioning" first.
func (s *Server) SetProvisioningPolls(n int) { s.With(func(st *State) { st.ProvisionPolls = n }) }

// SetProvisionOutcome sets the final item status of new orders ("active" or "failed").
func (s *Server) SetProvisionOutcome(status string) {
	s.With(func(st *State) { st.ProvisionOutcome = status })
}

// SetProvisioner registers a hook that enriches resources of resourceType created by orders.
func (s *Server) SetProvisioner(resourceType string, fn func(item, attrs map[string]any)) {
	s.With(func(st *State) { st.Provisioners[resourceType] = fn })
}

// SetOrderItemStatus forces the status of one item of an order.
func (s *Server) SetOrderItemStatus(orderID string, index int, status string) {
	s.With(func(st *State) {
		if o, ok := st.Orders[orderID]; ok && index >= 0 && index < len(o.Items) {
			o.Items[index].ForcedStatus = status
		}
	})
}

// MarkIdempotencyInProgress makes POST /orders with key answer 409 IDEMPOTENCY_IN_PROGRESS.
func (s *Server) MarkIdempotencyInProgress(key string) {
	s.With(func(st *State) { st.Idempotency[key] = &IdempotencyEntry{InProgress: true} })
}

// ChargeCount is the number of charged orders (replays do not count).
func (s *Server) ChargeCount() int {
	var n int
	s.With(func(st *State) { n = len(st.Charges) })
	return n
}

// SeedResource stores a resource and returns its id.
func (s *Server) SeedResource(typ string, attrs map[string]any) int64 {
	var id int64
	s.With(func(st *State) { id = st.AddResource(typ, attrs) })
	return id
}

// Resource returns a copy of a resource's attributes.
func (s *Server) Resource(typ string, id int64) (map[string]any, bool) {
	var (
		cp map[string]any
		ok bool
	)
	s.With(func(st *State) {
		var r map[string]any
		if r, ok = st.Resource(typ, id); ok {
			cp = copyMap(r)
		}
	})
	return cp, ok
}

// UpdateResource changes a resource in place; it reports whether it exists.
func (s *Server) UpdateResource(typ string, id int64, fn func(attrs map[string]any)) bool {
	var ok bool
	s.With(func(st *State) {
		var r map[string]any
		if r, ok = st.Resource(typ, id); ok {
			fn(r)
		}
	})
	return ok
}

// DeleteResource removes a resource (e.g. to simulate deletion outside Terraform).
func (s *Server) DeleteResource(typ string, id int64) {
	s.With(func(st *State) { delete(st.Resources[typ], id) })
}

// ResourceIDs returns the ids of a resource type in ascending order.
func (s *Server) ResourceIDs(typ string) []int64 {
	var ids []int64
	s.With(func(st *State) { ids = st.ResourceIDs(typ) })
	return ids
}
