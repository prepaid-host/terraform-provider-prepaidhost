// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Domain endpoints the domain and DNS record resources use, each answering
// like the real one (app/Http/Controllers/Api/DomainController.php,
// DomainManagementController.php, DnsRecordService):
//
//   - GET    /domain/{id}                    envelope {id, authcode, sld, tld, owner}; unknown → HTTP 200 {"success":false,"result":"Domain not found."}
//   - GET    /domain/{id}/nameservers        {nameservers, source}; unknown → abort(404) = NOT_FOUND
//   - PUT    /domain/{id}/nameservers        {"nameservers": [2–4] | null}; 422 VALIDATION_ERROR; result {nameservers}
//   - GET    /domain/{id}/renew              {price, years, balance, affordable, renewable, expires_at, auto_renew}; unknown or no rental → NOT_FOUND
//   - PUT    /domain/{id}/autorenew          {"enabled"} boolean; unknown or no rental → NOT_FOUND
//   - GET    /domain/{id}/dns                the zone (incl. the apex NS records); unknown domain → findOrFail = NOT_FOUND
//   - POST   /domain/{id}/dns                StoreDnsRecordRequest → 201 record
//   - PUT    /domain/{id}/dns/{recordId}     UpdateDnsRecordRequest → record; unknown record, NS or type change → 422 DNS_RECORD_REJECTED
//   - DELETE /domain/{id}/dns/{recordId}     → "deleted"; unknown record or NS → 422 DNS_RECORD_REJECTED
//
// Domain attributes in State.Resources["domain"]: sld, tld, name, authcode,
// owner, auto_renew (bool), created_at / expires_at (RFC 3339), nameservers
// ([]string), dns_records ([]map[string]any, host "" = zone root, like
// ClouDNS), no_rental (mock only: the domain has no rental row) and
// nameserver_source (mock only: "local" = the registrar did not answer).
func init() {
	register(func(s *Server) {
		s.Handle(http.MethodGet, "/domain/{id}", handleShowDomain)
		s.Handle(http.MethodGet, "/domain/{id}/nameservers", handleDomainNameservers)
		s.Handle(http.MethodPut, "/domain/{id}/nameservers", handleSetDomainNameservers)
		s.Handle(http.MethodGet, "/domain/{id}/renew", handleDomainRenewInfo)
		s.Handle(http.MethodPut, "/domain/{id}/autorenew", handleDomainAutoRenew)
		s.Handle(http.MethodGet, "/domain/{id}/dns", handleListDNS)
		s.Handle(http.MethodPost, "/domain/{id}/dns", handleCreateDNS)
		s.Handle(http.MethodPut, "/domain/{id}/dns/{recordId}", handleUpdateDNS)
		s.Handle(http.MethodDelete, "/domain/{id}/dns/{recordId}", handleDeleteDNS)
	})
}

// DefaultNameservers is NameserverService::DEFAULTS.
var DefaultNameservers = []string{"ns1.prepaid-host.com", "ns2.prepaid-host.com", "ns3.prepaid-host.com", "ns4.prepaid-host.com"}

// domainDurations is DomainConfigurator::DURATIONS.
var domainDurations = []int64{12, 24, 36}

// DNSTypes is DnsRecordService::SUPPORTED_TYPES, DNSTTLs its ALLOWED_TTLS.
var (
	DNSTypes = []string{"A", "AAAA", "CNAME", "MX", "TXT", "SRV"}
	DNSTTLs  = []int64{60, 300, 600, 900, 1800, 3600, 21600, 43200, 86400, 172800, 259200, 604800, 1209600, 2592000}
)

var nameserverRE = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+\.?$`)

func notFoundModel() Response {
	return Fail(http.StatusNotFound, "NOT_FOUND", "Ressource nicht gefunden.", nil)
}

func dnsRejected(message string) Response {
	return Fail(http.StatusUnprocessableEntity, "DNS_RECORD_REJECTED", message, nil)
}

func (st *State) domain(r *Request) (map[string]any, bool) {
	id, ok := r.ParamInt64("id")
	if !ok {
		return nil, false
	}
	return st.Resource(ResourceDomain, id)
}

func hasRental(attrs map[string]any) bool {
	b, _ := attrs["no_rental"].(bool)
	return !b
}

// ---- ordering (DomainConfigurator in API mode) ----

// validateDomainItem checks a domain item like DomainConfigurator::configure
// in API mode: the rules in their order (the first failing one is reported),
// then the TLD, then the availability of a register.
func (st *State) validateDomainItem(index int, it map[string]any) *Response {
	invalid := func(key, message string) *Response {
		resp := invalidField(fmt.Sprintf("items.%d.%s", index, key), message)
		return &resp
	}
	// A transfer is refused before any other rule (the server checks the
	// action first): nothing else of the item matters then.
	if a, _ := it["action"].(string); a == "transfer" {
		return invalid("action", "Domain-Transfers sind per API noch nicht möglich – bitte im Kundenpanel durchführen.")
	}
	sld, _ := it["sld"].(string)
	if sld == "" || len(sld) > 63 {
		return invalid("sld", "The sld field is required.")
	}
	tld, _ := it["tld"].(string)
	if tld == "" {
		return invalid("tld", "The tld field is required.")
	}
	if !slices.Contains(domainDurations, toInt(it["duration_months"])) || !isIntegral(it["duration_months"]) {
		return invalid("duration_months", "The selected duration months is invalid.")
	}
	if v, ok := it["action"]; ok && v != nil {
		if a, _ := v.(string); a != "register" {
			return invalid("action", "The selected action is invalid.")
		}
	}
	if it["contact_id"] == nil {
		return invalid("contact_id", "The contact id field is required.")
	}
	if _, ok := st.Resource(ResourceDomainContact, toInt(it["contact_id"])); !ok || !isIntegral(it["contact_id"]) {
		return invalid("contact_id", "The selected contact id is invalid.")
	}
	if !st.knownTLD(tld) {
		return invalid("tld", "Ungültige TLD.")
	}
	// A register is checked at the registrar, where every registered domain
	// is taken — also the caller's own. The check is skipped only when the
	// caller holds a live API-ordered item with this name as client
	// reference: the order then replays (same key) or answers
	// CLIENT_REFERENCE_IN_USE. A domain registered in the panel has no such
	// item, so it stays unavailable.
	name := domainName(it)
	if _, _, held := st.referenceInUse(name); !held && (st.UnavailableDomains[name] || st.domainRegistered(name)) {
		resp := Fail(http.StatusUnprocessableEntity, "DOMAIN_UNAVAILABLE", fmt.Sprintf("Die Domain %s ist nicht registrierbar.", name),
			map[string]any{"domain": name})
		return &resp
	}
	return nil
}

// domainRegistered reports whether a domain of this name exists in the mock
// (i.e. it is registered at the "registrar").
func (st *State) domainRegistered(name string) bool {
	for _, id := range st.ResourceIDs(ResourceDomain) {
		if st.Resources[ResourceDomain][id]["name"] == name {
			return true
		}
	}
	return false
}

func isIntegral(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == math.Trunc(x)
	case int, int64:
		return true
	}
	return false
}

// knownTLD reports whether tld is an entry of the domain catalog in use.
func (st *State) knownTLD(tld string) bool {
	c, ok := st.Catalogs["domain"]
	if !ok {
		c = defaultCatalog("domain")
	}
	tlds, _ := c["tlds"].([]any)
	for _, e := range tlds {
		if m, ok := e.(map[string]any); ok && strings.EqualFold(fmt.Sprint(m["tld"]), tld) {
			return true
		}
	}
	return false
}

// fulfillDomain completes a domain created by an order like DomainFulfiller
// plus the registration job: owner from the contact, the platform's
// nameservers and a zone with the four apex NS records.
func (st *State) fulfillDomain(attrs map[string]any) {
	if c, ok := st.Resource(ResourceDomainContact, toInt(attrs["contact_id"])); ok {
		attrs["owner"] = map[string]any{"firstname": c["firstname"], "lastname": c["lastname"], "email": c["email"]}
	}
	st.initZone(attrs)
}

func (st *State) initZone(attrs map[string]any) {
	if _, ok := attrs["nameservers"]; !ok {
		attrs["nameservers"] = slices.Clone(DefaultNameservers)
	}
	if _, ok := attrs["dns_records"]; !ok {
		records := []map[string]any{}
		for _, ns := range DefaultNameservers {
			records = append(records, map[string]any{"id": st.NextID(), "type": "NS", "host": "", "record": ns, "ttl": int64(3600)})
		}
		attrs["dns_records"] = records
	}
}

// SeedDomain stores a live domain (example.de, expires in a year, auto-renew
// on, the platform's nameservers, a zone with the apex NS records) with
// attrs overriding the defaults, and returns its id.
func (s *Server) SeedDomain(attrs map[string]any) int64 {
	var id int64
	s.With(func(st *State) {
		now := time.Now().UTC()
		a := map[string]any{
			"sld": "example", "tld": "de", "auto_renew": true, "owner": map[string]any{},
			"created_at": now.Format(time.RFC3339), "expires_at": now.AddDate(1, 0, 0).Format(time.RFC3339),
		}
		for k, v := range attrs {
			a[k] = v
		}
		if _, ok := a["name"]; !ok {
			a["name"] = domainName(a)
		}
		st.initZone(a)
		id = st.AddResource(ResourceDomain, a)
	})
	return id
}

// DNSRecords returns a copy of a domain's zone in the API's representation.
func (s *Server) DNSRecords(domainID int64) []map[string]any {
	var out []map[string]any
	s.With(func(st *State) {
		if d, ok := st.Resource(ResourceDomain, domainID); ok {
			for _, r := range zone(d) {
				out = append(out, presentDNS(r))
			}
		}
	})
	return out
}

// DeleteDNSRecord removes a record from a zone (e.g. deleted in the panel).
func (s *Server) DeleteDNSRecord(domainID, recordID int64) {
	s.With(func(st *State) {
		if d, ok := st.Resource(ResourceDomain, domainID); ok {
			d["dns_records"] = slices.DeleteFunc(zone(d), func(r map[string]any) bool { return toInt(r["id"]) == recordID })
		}
	})
}

// UpdateDNSRecord changes a stored record in place (e.g. edited in the panel).
func (s *Server) UpdateDNSRecord(domainID, recordID int64, fn func(rec map[string]any)) {
	s.With(func(st *State) {
		if d, ok := st.Resource(ResourceDomain, domainID); ok {
			for _, r := range zone(d) {
				if toInt(r["id"]) == recordID {
					fn(r)
				}
			}
		}
	})
}

// ---- show, renew, autorenew ----

func handleShowDomain(st *State, r *Request) Response {
	attrs, ok := st.domain(r)
	if !ok {
		return Legacy(http.StatusOK, "Domain not found.")
	}
	return OK(map[string]any{
		"id": attrs["id"], "authcode": attrs["authcode"], "sld": attrs["sld"], "tld": attrs["tld"], "owner": attrs["owner"],
	})
}

func handleDomainRenewInfo(st *State, r *Request) Response {
	attrs, ok := st.domain(r)
	if !ok || !hasRental(attrs) {
		return abort404()
	}
	price := round2(st.Prices["domain"])
	var expires any
	renewable := false
	if t, err := time.Parse(time.RFC3339, fmt.Sprint(attrs["expires_at"])); err == nil {
		expires = t.UTC().Format("2006-01-02T15:04:05-07:00")
		renewable = !time.Now().After(t)
	}
	autoRenew, _ := attrs["auto_renew"].(bool)
	return OK(map[string]any{
		"price": price, "years": 1, "balance": round2(st.Balance), "affordable": st.Balance >= price,
		"renewable": renewable, "expires_at": expires, "auto_renew": autoRenew,
	})
}

func handleDomainAutoRenew(st *State, r *Request) Response {
	attrs, ok := st.domain(r)
	if !ok || !hasRental(attrs) {
		return abort404()
	}
	v, present := r.BodyMap()["enabled"]
	if !present || v == nil {
		return validationError("enabled", "The enabled field is required.")
	}
	enabled, ok := laravelBoolean(v)
	if !ok {
		return validationError("enabled", "The enabled field must be true or false.")
	}
	attrs["auto_renew"] = enabled
	return OK(map[string]any{"auto_renew": enabled})
}

// ---- nameservers ----

func handleDomainNameservers(st *State, r *Request) Response {
	attrs, ok := st.domain(r)
	if !ok {
		return abort404()
	}
	ns, _ := attrs["nameservers"].([]string)
	// NameserverService::get falls back to the local ns1/ns2 columns when the
	// registrar has no answer (mock: nameserver_source = "local").
	if attrs["nameserver_source"] == "local" {
		return OK(map[string]any{"nameservers": append([]string{}, ns[:min(2, len(ns))]...), "source": "local"})
	}
	return OK(map[string]any{"nameservers": append([]string{}, ns...), "source": "registrar"})
}

// normaliseNameservers is UpdateNameserversRequest::normaliseNameservers
// (without the IDN conversion): whitespace, scheme, path and port removed,
// lower-cased, trailing dot dropped, empty entries and duplicates removed.
func normaliseNameservers(raw []any) []string {
	out := []string{}
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.Join(strings.Fields(s), "")
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		if i := strings.IndexAny(s, "/?#"); i >= 0 {
			s = s[:i]
		}
		s = strings.Trim(s, "[]")
		if h, _, err := net.SplitHostPort(s); err == nil {
			s = h
		}
		s = strings.TrimSuffix(strings.ToLower(s), ".")
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func handleSetDomainNameservers(st *State, r *Request) Response {
	attrs, ok := st.domain(r)
	if !ok {
		return abort404()
	}
	body := r.BodyMap()
	raw, present := body["nameservers"]
	if !present || raw == nil {
		attrs["nameservers"] = slices.Clone(DefaultNameservers)
		return OK(map[string]any{"nameservers": DefaultNameservers})
	}
	list, isList := raw.([]any)
	if !isList {
		return validationError("nameservers", "The nameservers field must be an array.")
	}
	ns := normaliseNameservers(list)
	if len(ns) < 2 || len(ns) > 4 {
		return validationError("nameservers", "The nameservers field must have between 2 and 4 items.")
	}
	for i, n := range ns {
		if len(n) > 255 || !nameserverRE.MatchString(n) || net.ParseIP(strings.TrimSuffix(n, ".")) != nil {
			return validationError(fmt.Sprintf("nameservers.%d", i), "Ungültiger Nameserver.")
		}
	}
	attrs["nameservers"] = ns
	return OK(map[string]any{"nameservers": ns})
}

// ---- DNS ----

func zone(d map[string]any) []map[string]any {
	z, _ := d["dns_records"].([]map[string]any)
	return z
}

// presentDNS is DnsRecordService::normalise: root host as "@", priority null
// unless set, weight and port only for SRV.
func presentDNS(rec map[string]any) map[string]any {
	host, _ := rec["host"].(string)
	if host == "" {
		host = "@"
	}
	out := map[string]any{
		"id": toInt(rec["id"]), "type": rec["type"], "host": host, "record": rec["record"],
		"ttl": toInt(rec["ttl"]), "priority": nil, "status": 1,
	}
	if p, ok := rec["priority"]; ok && p != nil {
		out["priority"] = toInt(p)
	}
	if rec["type"] == "SRV" {
		out["weight"], out["port"] = rec["weight"], rec["port"]
	}
	return out
}

func handleListDNS(st *State, r *Request) Response {
	d, ok := st.domain(r)
	if !ok {
		return notFoundModel()
	}
	out := []map[string]any{}
	for _, rec := range zone(d) {
		out = append(out, presentDNS(rec))
	}
	return OK(out)
}

// dnsFieldErrors validates the DNS fields that are present like the
// FormRequests (Laravel's integer rule: integral numbers or numeric strings).
func dnsFieldErrors(body map[string]any, creating bool) map[string]any {
	errs := map[string]any{}
	fail := func(field, msg string) { errs[field] = []string{msg} }
	// intIn reports whether the field is present (not null).
	intIn := func(field string, lo, hi int64) bool {
		v, ok := body[field]
		if !ok || v == nil {
			return false
		}
		n, isInt := laravelInt(v)
		if !isInt {
			fail(field, "The "+field+" field must be an integer.")
		} else if n < lo || n > hi {
			fail(field, fmt.Sprintf("The %s field must be between %d and %d.", field, lo, hi))
		}
		return true
	}
	if h, ok := body["host"]; ok && h != nil {
		if s, isStr := h.(string); !isStr || len(s) > 255 {
			fail("host", "The host field must be a string of at most 255 characters.")
		}
	}
	typ, _ := body["type"].(string)
	if _, present := body["type"]; creating || present {
		if !slices.Contains(DNSTypes, typ) {
			fail("type", "Nur diese Typen werden unterstützt: "+strings.Join(DNSTypes, ", "))
		}
	}
	if rec, present := body["record"]; creating || present {
		if s, isStr := rec.(string); !isStr || s == "" || len(s) > 2048 {
			fail("record", "The record field is required and must be a string of at most 2048 characters.")
		}
	}
	if v, present := body["ttl"]; present && (v != nil || !creating) {
		if n, isInt := laravelInt(v); !isInt || !slices.Contains(DNSTTLs, n) {
			fail("ttl", "Diese TTL wird nicht unterstützt.")
		}
	}
	intIn("priority", 0, 65535)
	hasWeight := intIn("weight", 0, 65535)
	hasPort := intIn("port", 1, 65535)
	if creating && typ == "SRV" {
		if !hasWeight && errs["weight"] == nil {
			fail("weight", "The weight field is required when type is SRV.")
		}
		if !hasPort && errs["port"] == nil {
			fail("port", "The port field is required when type is SRV.")
		}
	}
	return errs
}

func laravelInt(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) {
			return int64(x), true
		}
	case string:
		var n int64
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil && fmt.Sprint(n) == x {
			return n, true
		}
	}
	return 0, false
}

func dnsValidationError(errs map[string]any) Response {
	return Fail(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Validierungsfehler.", map[string]any{"details": errs})
}

// resolvePriority is DnsRecordService::resolvePriority.
func resolvePriority(typ string, v any) any {
	if typ != "MX" && typ != "SRV" {
		return nil
	}
	if v == nil || v == "" {
		if typ == "MX" {
			return int64(10)
		}
		return int64(0)
	}
	n, _ := laravelInt(v)
	return n
}

func rootHost(v any) string {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if s == "@" {
		return ""
	}
	return s
}

func handleCreateDNS(st *State, r *Request) Response {
	d, ok := st.domain(r)
	if !ok {
		return notFoundModel()
	}
	body := r.BodyMap()
	// prepareForValidation: legacy aliases, type upper-cased.
	if _, ok := body["host"]; !ok {
		if v, ok := body["sld"]; ok {
			body["host"] = v
		} else {
			body["host"] = "@"
		}
	}
	if _, ok := body["record"]; !ok {
		body["record"] = body["data"]
	}
	if s, ok := body["type"].(string); ok {
		body["type"] = strings.ToUpper(s)
	}
	if errs := dnsFieldErrors(body, true); len(errs) > 0 {
		return dnsValidationError(errs)
	}
	typ := body["type"].(string) //nolint:forcetypeassert // validated
	ttl := int64(300)
	if v, ok := body["ttl"]; ok && v != nil {
		ttl, _ = laravelInt(v)
	}
	rec := map[string]any{
		"id": st.NextID(), "type": typ, "host": rootHost(body["host"]), "record": body["record"], "ttl": ttl,
		"priority": resolvePriority(typ, body["priority"]),
	}
	if typ == "SRV" {
		w, _ := laravelInt(body["weight"])
		p, _ := laravelInt(body["port"])
		rec["weight"], rec["port"] = w, p
	}
	d["dns_records"] = append(zone(d), rec)
	return Created(presentDNS(rec))
}

func findRecord(d map[string]any, r *Request) (map[string]any, bool) {
	id, ok := r.ParamInt64("recordId")
	if !ok {
		return nil, false
	}
	for _, rec := range zone(d) {
		if toInt(rec["id"]) == id {
			return rec, true
		}
	}
	return nil, false
}

func handleUpdateDNS(st *State, r *Request) Response {
	d, ok := st.domain(r)
	if !ok {
		return notFoundModel()
	}
	body := r.BodyMap()
	if _, ok := body["host"]; !ok {
		if v, ok := body["sld"]; ok {
			body["host"] = v
		}
	}
	if _, ok := body["record"]; !ok {
		if v, ok := body["data"]; ok {
			body["record"] = v
		}
	}
	if s, ok := body["type"].(string); ok {
		body["type"] = strings.ToUpper(s)
	}
	errs := dnsFieldErrors(body, false)
	changeable := false
	for _, f := range []string{"host", "record", "ttl", "priority", "weight", "port"} {
		if _, ok := body[f]; ok {
			changeable = true
		}
	}
	if !changeable {
		errs["record"] = []string{"Es wurde nichts zum Ändern übergeben."}
	}
	if len(errs) > 0 {
		return dnsValidationError(errs)
	}
	rec, ok := findRecord(d, r)
	if !ok {
		return dnsRejected("Der DNS-Eintrag wurde nicht gefunden.")
	}
	if rec["type"] == "NS" {
		return dnsRejected("Nur diese Typen werden unterstützt: " + strings.Join(DNSTypes, ", "))
	}
	if t, ok := body["type"]; ok && t != rec["type"] {
		return dnsRejected("Der Typ eines Eintrags kann nicht geändert werden.")
	}
	typ, _ := rec["type"].(string)
	if v, ok := body["host"]; ok {
		rec["host"] = rootHost(v)
	}
	if v, ok := body["record"]; ok {
		rec["record"] = v
	}
	if v, ok := body["ttl"]; ok && v != nil {
		rec["ttl"], _ = laravelInt(v)
	}
	if v, ok := body["priority"]; ok {
		rec["priority"] = resolvePriority(typ, v)
	}
	if typ == "SRV" {
		for _, f := range []string{"weight", "port"} {
			if v, ok := body[f]; ok && v != nil {
				rec[f], _ = laravelInt(v)
			}
		}
	}
	return OK(presentDNS(rec))
}

func handleDeleteDNS(st *State, r *Request) Response {
	d, ok := st.domain(r)
	if !ok {
		return notFoundModel()
	}
	rec, ok := findRecord(d, r)
	if !ok {
		return dnsRejected("Der DNS-Eintrag wurde nicht gefunden.")
	}
	if rec["type"] == "NS" {
		return dnsRejected("Nur diese Typen werden unterstützt: " + strings.Join(DNSTypes, ", "))
	}
	id := toInt(rec["id"])
	d["dns_records"] = slices.DeleteFunc(zone(d), func(x map[string]any) bool { return toInt(x["id"]) == id })
	return OK("deleted")
}
