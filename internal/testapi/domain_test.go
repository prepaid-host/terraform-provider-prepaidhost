// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

func domainOrderItem(api *testapi.Server, sld string) map[string]any {
	contact := api.SeedResource(testapi.ResourceDomainContact, map[string]any{"name": "Max", "email": "max@example.com"})
	return map[string]any{"type": "domain", "sld": sld, "tld": "de", "duration_months": 12, "contact_id": contact}
}

func quoteError(t *testing.T, api *testapi.Server, it map[string]any) (int, map[string]any) {
	t.Helper()
	resp, env := call(t, api, http.MethodPost, "/orders/quote", api.Token, map[string]any{"items": []any{it}}, nil)
	return resp.StatusCode, env.Error
}

// DomainConfigurator (API mode): the rules in their order, then the TLD,
// then the availability of a register.
func TestDomainOrderValidation(t *testing.T) {
	api := testapi.New(t)
	api.SetDomainUnavailable("taken.de")
	// Registered, so taken at the registrar: one from the panel, one ordered
	// through the API (live client reference) — only the latter may be ordered again.
	api.SeedDomain(map[string]any{"sld": "panel"})
	api.SeedDomain(map[string]any{"sld": "mine", "client_reference": "mine.de"})
	cases := map[string]struct {
		change map[string]any
		code   string
		field  string
	}{
		"duration":           {map[string]any{"duration_months": 6}, "VALIDATION_FAILED", "items.0.duration_months"},
		"no duration":        {map[string]any{"duration_months": nil}, "VALIDATION_FAILED", "items.0.duration_months"},
		"action":             {map[string]any{"action": "renew"}, "VALIDATION_FAILED", "items.0.action"},
		"transfer":           {map[string]any{"action": "transfer", "authcode": "A1"}, "VALIDATION_FAILED", "items.0.action"},
		"no contact":         {map[string]any{"contact_id": nil}, "VALIDATION_FAILED", "items.0.contact_id"},
		"unknown contact":    {map[string]any{"contact_id": 999999}, "VALIDATION_FAILED", "items.0.contact_id"},
		"unknown tld":        {map[string]any{"tld": "xyz"}, "VALIDATION_FAILED", "items.0.tld"},
		"taken":              {map[string]any{"sld": "taken"}, "DOMAIN_UNAVAILABLE", ""},
		"registered (panel)": {map[string]any{"sld": "panel"}, "DOMAIN_UNAVAILABLE", ""},
		"own API reference":  {map[string]any{"sld": "mine"}, "", ""},
	}
	for name, tc := range cases {
		it := domainOrderItem(api, "example")
		for k, v := range tc.change {
			if v == nil {
				delete(it, k)
			} else {
				it[k] = v
			}
		}
		status, e := quoteError(t, api, it)
		if tc.code == "" {
			if status != http.StatusOK {
				t.Errorf("%s: status %d %v", name, status, e)
			}
			continue
		}
		if status != http.StatusUnprocessableEntity || e["code"] != tc.code || (tc.field != "" && e["field"] != tc.field) {
			t.Errorf("%s: status=%d error=%v", name, status, e)
		}
	}
}

// The client reference of a domain is always its name (API mode), and the
// TLD keeps its case (only the sld is lower-cased).
func TestDomainOrderReferenceIsTheName(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(100)
	it := domainOrderItem(api, "Example")
	it["client_reference"] = "something-else"
	resp, env := call(t, api, http.MethodPost, "/orders", api.Token, map[string]any{"items": []any{it}, "expected_total": 12.0},
		map[string]string{"Idempotency-Key": "key-domain-ref"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d %v", resp.StatusCode, env.Error)
	}
	ids := api.ResourceIDs(testapi.ResourceDomain)
	res, _ := api.Resource(testapi.ResourceDomain, ids[0])
	if res["client_reference"] != "example.de" || res["name"] != "example.de" {
		t.Errorf("resource = %v", res)
	}
}

func TestDomainShow(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedDomain(map[string]any{"sld": "shop", "tld": "co.uk", "authcode": "SECRET"})
	_, env := call(t, api, http.MethodGet, fmt.Sprintf("/domain/%d", id), api.Token, nil, nil)
	var d map[string]any
	_ = json.Unmarshal(env.Result, &d)
	if !env.Success || d["sld"] != "shop" || d["tld"] != "co.uk" || d["authcode"] != "SECRET" || len(d) != 5 {
		t.Errorf("show = %v (want exactly id, authcode, sld, tld, owner)", d)
	}

	// Unknown: legacy HTTP 200.
	resp, env := call(t, api, http.MethodGet, "/domain/424242", api.Token, nil, nil)
	if resp.StatusCode != http.StatusOK || env.Success || !strings.Contains(string(env.Result), "Domain not found.") {
		t.Errorf("unknown: %d %+v", resp.StatusCode, env)
	}
}

func TestDomainRenewAndAutoRenew(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedDomain(map[string]any{"expires_at": "2030-01-02T03:04:05Z"})
	p := fmt.Sprintf("/domain/%d", id)
	_, env := call(t, api, http.MethodGet, p+"/renew", api.Token, nil, nil)
	var r map[string]any
	_ = json.Unmarshal(env.Result, &r)
	if r["expires_at"] != "2030-01-02T03:04:05+00:00" || r["auto_renew"] != true || r["years"] != float64(1) || r["renewable"] != true {
		t.Errorf("renew = %v", r)
	}

	resp, env := call(t, api, http.MethodPut, p+"/autorenew", api.Token, map[string]any{"enabled": "x"}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "VALIDATION_ERROR" {
		t.Errorf("bad enabled: %d %v", resp.StatusCode, env.Error)
	}
	resp, _ = call(t, api, http.MethodPut, p+"/autorenew", api.Token, map[string]any{"enabled": false}, nil)
	if res, _ := api.Resource(testapi.ResourceDomain, id); resp.StatusCode != http.StatusOK || res["auto_renew"] != false {
		t.Errorf("autorenew off: %d %v", resp.StatusCode, res["auto_renew"])
	}

	for _, path := range []string{"/domain/424242/renew", "/domain/424242/nameservers"} {
		resp, env = call(t, api, http.MethodGet, path, api.Token, nil, nil)
		if resp.StatusCode != http.StatusNotFound || env.Error["code"] != "NOT_FOUND" {
			t.Errorf("%s: %d %v", path, resp.StatusCode, env.Error)
		}
	}

	// A domain without rental: renew and autorenew answer 404.
	bare := api.SeedDomain(map[string]any{"sld": "bare", "no_rental": true})
	resp, _ = call(t, api, http.MethodGet, fmt.Sprintf("/domain/%d/renew", bare), api.Token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no rental: %d", resp.StatusCode)
	}

	expired := api.SeedDomain(map[string]any{"sld": "old", "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	_, env = call(t, api, http.MethodGet, fmt.Sprintf("/domain/%d/renew", expired), api.Token, nil, nil)
	_ = json.Unmarshal(env.Result, &r)
	if r["renewable"] != false {
		t.Errorf("expired domain renewable = %v", r["renewable"])
	}
}

func TestDomainNameservers(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedDomain(nil)
	p := fmt.Sprintf("/domain/%d/nameservers", id)
	_, env := call(t, api, http.MethodGet, p, api.Token, nil, nil)
	var got struct {
		Nameservers []string `json:"nameservers"`
		Source      string   `json:"source"`
	}
	_ = json.Unmarshal(env.Result, &got)
	if fmt.Sprint(got.Nameservers) != "[ns1.prepaid-host.com ns2.prepaid-host.com ns3.prepaid-host.com ns4.prepaid-host.com]" || got.Source != "registrar" {
		t.Errorf("default nameservers = %+v", got)
	}

	// Normalised like UpdateNameserversRequest::normaliseNameservers.
	resp, env := call(t, api, http.MethodPut, p, api.Token, map[string]any{"nameservers": []any{" NS1.Example.COM. ", "ns2.example.com", "ns2.example.com"}}, nil)
	_ = json.Unmarshal(env.Result, &got)
	if resp.StatusCode != http.StatusOK || fmt.Sprint(got.Nameservers) != "[ns1.example.com ns2.example.com]" {
		t.Errorf("set: %d %+v %v", resp.StatusCode, got, env.Error)
	}
	if res, _ := api.Resource(testapi.ResourceDomain, id); fmt.Sprint(res["nameservers"]) != "[ns1.example.com ns2.example.com]" {
		t.Errorf("stored = %v", res["nameservers"])
	}

	for name, body := range map[string]any{
		"one":     []any{"ns1.example.com"},
		"five":    []any{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com"},
		"ip":      []any{"ns1.example.com", "192.0.2.1"},
		"invalid": []any{"ns1.example.com", "-bad-.example.com"},
	} {
		resp, env = call(t, api, http.MethodPut, p, api.Token, map[string]any{"nameservers": body}, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "VALIDATION_ERROR" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, env.Error)
		}
	}

	// Registrar fallback: the local ns1/ns2, marked as such.
	api.UpdateResource(testapi.ResourceDomain, id, func(a map[string]any) { a["nameserver_source"] = "local" })
	_, env = call(t, api, http.MethodGet, p, api.Token, nil, nil)
	_ = json.Unmarshal(env.Result, &got)
	if got.Source != "local" || len(got.Nameservers) != 2 {
		t.Errorf("local fallback = %+v", got)
	}

	// null resets to the platform's nameservers.
	_, env = call(t, api, http.MethodPut, p, api.Token, map[string]any{"nameservers": nil}, nil)
	_ = json.Unmarshal(env.Result, &got)
	if len(got.Nameservers) != 4 {
		t.Errorf("reset = %+v", got)
	}
}

type dnsRecord struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Record   string `json:"record"`
	TTL      int64  `json:"ttl"`
	Priority *int64 `json:"priority"`
	Weight   *int64 `json:"weight"`
	Port     *int64 `json:"port"`
}

func listDNS(t *testing.T, api *testapi.Server, id int64) []dnsRecord {
	t.Helper()
	_, env := call(t, api, http.MethodGet, fmt.Sprintf("/domain/%d/dns", id), api.Token, nil, nil)
	var list []dnsRecord
	if err := json.Unmarshal(env.Result, &list); err != nil {
		t.Fatalf("list: %v %s", err, env.Result)
	}
	return list
}

func TestDNSRecords(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedDomain(nil)
	p := fmt.Sprintf("/domain/%d/dns", id)

	// The zone starts with the four apex NS records.
	if list := listDNS(t, api, id); len(list) != 4 || list[0].Type != "NS" || list[0].Host != "@" {
		t.Fatalf("initial zone = %+v", list)
	}

	// Create: type upper-cased, root host "@", ttl default 300, MX priority default 10.
	resp, env := call(t, api, http.MethodPost, p, api.Token, map[string]any{"type": "mx", "record": "mail.example.com"}, nil)
	var mx dnsRecord
	_ = json.Unmarshal(env.Result, &mx)
	if resp.StatusCode != http.StatusCreated || mx.ID == 0 || mx.Type != "MX" || mx.Host != "@" || mx.TTL != 300 || mx.Priority == nil || *mx.Priority != 10 {
		t.Errorf("create MX: %d %+v", resp.StatusCode, mx)
	}
	_, env = call(t, api, http.MethodPost, p, api.Token, map[string]any{"type": "A", "host": "www", "record": "192.0.2.1", "ttl": 3600, "priority": 5}, nil)
	var a dnsRecord
	_ = json.Unmarshal(env.Result, &a)
	if a.Host != "www" || a.TTL != 3600 || a.Priority != nil {
		t.Errorf("create A (priority ignored) = %+v", a)
	}
	_, env = call(t, api, http.MethodPost, p, api.Token, map[string]any{"type": "SRV", "host": "_sip._tcp", "record": "sip.example.com", "weight": 5, "port": 5060}, nil)
	var srv dnsRecord
	_ = json.Unmarshal(env.Result, &srv)
	if srv.Priority == nil || *srv.Priority != 0 || srv.Weight == nil || *srv.Weight != 5 || srv.Port == nil || *srv.Port != 5060 {
		t.Errorf("create SRV = %+v", srv)
	}

	for name, body := range map[string]map[string]any{
		"no type":      {"record": "x"},
		"NS":           {"type": "NS", "record": "ns.example.com"},
		"no record":    {"type": "A"},
		"bad ttl":      {"type": "A", "record": "192.0.2.1", "ttl": 301},
		"priority":     {"type": "MX", "record": "m", "priority": 70000},
		"srv w/o port": {"type": "SRV", "record": "s", "weight": 1},
	} {
		resp, env = call(t, api, http.MethodPost, p, api.Token, body, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "VALIDATION_ERROR" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, env.Error)
		}
	}

	// Update: partial, type is immutable, unknown id → DNS_RECORD_REJECTED.
	rp := fmt.Sprintf("%s/%d", p, a.ID)
	resp, env = call(t, api, http.MethodPut, rp, api.Token, map[string]any{"record": "192.0.2.2"}, nil)
	var upd dnsRecord
	_ = json.Unmarshal(env.Result, &upd)
	if resp.StatusCode != http.StatusOK || upd.Record != "192.0.2.2" || upd.TTL != 3600 || upd.Host != "www" || upd.ID != a.ID {
		t.Errorf("update: %d %+v", resp.StatusCode, upd)
	}
	for name, tc := range map[string]struct {
		path string
		body map[string]any
		code string
	}{
		"type change": {rp, map[string]any{"type": "AAAA", "record": "::1"}, "DNS_RECORD_REJECTED"},
		"unknown id":  {p + "/999999", map[string]any{"record": "x"}, "DNS_RECORD_REJECTED"},
		"NS":          {fmt.Sprintf("%s/%d", p, listDNS(t, api, id)[0].ID), map[string]any{"record": "x"}, "DNS_RECORD_REJECTED"},
		"nothing":     {rp, map[string]any{}, "VALIDATION_ERROR"},
	} {
		resp, env = call(t, api, http.MethodPut, tc.path, api.Token, tc.body, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != tc.code {
			t.Errorf("%s: %d %v", name, resp.StatusCode, env.Error)
		}
	}

	// Delete.
	resp, _ = call(t, api, http.MethodDelete, rp, api.Token, nil, nil)
	if resp.StatusCode != http.StatusOK || len(listDNS(t, api, id)) != 6 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	resp, env = call(t, api, http.MethodDelete, rp, api.Token, nil, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || env.Error["code"] != "DNS_RECORD_REJECTED" {
		t.Errorf("delete twice: %d %v", resp.StatusCode, env.Error)
	}

	// Unknown domain: findOrFail → NOT_FOUND.
	resp, env = call(t, api, http.MethodGet, "/domain/424242/dns", api.Token, nil, nil)
	if resp.StatusCode != http.StatusNotFound || env.Error["code"] != "NOT_FOUND" {
		t.Errorf("unknown domain: %d %v", resp.StatusCode, env.Error)
	}
}
