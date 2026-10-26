// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

const domainAddr = "prepaidhost_domain.main"

// domainTest is a mock API with a balance and one domain contact.
type domainTest struct {
	api     *testapi.Server
	contact int64
}

func newDomainTest(t *testing.T) domainTest {
	t.Helper()
	api := testapi.New(t)
	api.SetBalance(100)
	contact := api.SeedResource(testapi.ResourceDomainContact, map[string]any{"firstname": "Max", "lastname": "Muster", "email": "max@example.com"})
	return domainTest{api: api, contact: contact}
}

// config renders the test domain; set overrides or adds arguments (raw HCL),
// "-" removes one.
func (d domainTest) config(set map[string]string) string {
	args := [][2]string{
		{"name", `"example.de"`},
		{"contact_id", strconv.FormatInt(d.contact, 10)},
		{"create_timeout", `"20s"`},
	}
	var b strings.Builder
	b.WriteString(providerConfig(d.api))
	b.WriteString("resource \"prepaidhost_domain\" \"main\" {\n")
	seen := map[string]bool{}
	for _, kv := range args {
		seen[kv[0]] = true
		v := kv[1]
		if o, ok := set[kv[0]]; ok {
			v = o
		}
		if v != "-" {
			fmt.Fprintf(&b, "  %s = %s\n", kv[0], v)
		}
	}
	extra := make([]string, 0, len(set))
	for k := range set {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		if set[k] != "-" {
			fmt.Fprintf(&b, "  %s = %s\n", k, set[k])
		}
	}
	b.WriteString("}\n")
	return b.String()
}

func domainID(t *testing.T, api *testapi.Server) int64 {
	t.Helper()
	ids := api.ResourceIDs(testapi.ResourceDomain)
	if len(ids) != 1 {
		t.Fatalf("domains = %v, want exactly one", ids)
	}
	return ids[0]
}

func domainAttr(api *testapi.Server, id int64, key string) any {
	r, _ := api.Resource(testapi.ResourceDomain, id)
	return r[key]
}

func TestAccDomain_Register(t *testing.T) {
	d := newDomainTest(t)
	accTest(t, tfresource.TestStep{
		Config: d.config(nil),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttrSet(domainAddr, "id"),
			tfresource.TestCheckResourceAttr(domainAddr, "status", "active"),
			tfresource.TestCheckResourceAttr(domainAddr, "auto_renew", "true"),
			tfresource.TestCheckResourceAttr(domainAddr, "action", "register"),
			tfresource.TestCheckResourceAttr(domainAddr, "duration_months", "12"),
			tfresource.TestCheckResourceAttr(domainAddr, "nameservers.#", "4"),
			tfresource.TestCheckResourceAttr(domainAddr, "nameservers.0", "ns1.prepaid-host.com"),
			tfresource.TestCheckNoResourceAttr(domainAddr, "authcode"),
			tfresource.TestCheckResourceAttrWith(domainAddr, "expires_at", func(v string) error {
				exp, err := time.Parse(time.RFC3339, v)
				if err != nil || exp.Before(time.Now().AddDate(0, 11, 0)) {
					return fmt.Errorf("expires_at = %q (%v)", v, err)
				}
				return nil
			}),
			wantCharges(d.api, 1),
			checkMock(func() error {
				it := lastOrderItem(t, d.api)
				want := []string{"action", "client_reference", "contact_id", "duration_months", "sld", "tld", "type"}
				if got := keysOf(it); fmt.Sprint(got) != fmt.Sprint(want) {
					return fmt.Errorf("item fields = %v, want %v (the item fields the API evaluates for a domain, no authcode)", got, want)
				}
				if it["type"] != "domain" || it["sld"] != "example" || it["tld"] != "de" || it["client_reference"] != "example.de" ||
					it["action"] != "register" || it["duration_months"] != float64(12) || it["contact_id"] != float64(d.contact) {
					return fmt.Errorf("item = %v", it)
				}
				if n := d.api.CallCount(http.MethodPut, "/domain/{id}/nameservers"); n != 0 {
					return fmt.Errorf("nameserver PUTs = %d without configured nameservers", n)
				}
				return nil
			}),
		),
	}, tfresource.TestStep{Config: d.config(nil), PlanOnly: true})
}

// The sld is a single label, so everything after the first dot is the TLD.
func TestAccDomain_MultiLabelTLD(t *testing.T) {
	d := newDomainTest(t)
	d.api.SetCatalog("domain", map[string]any{
		"tlds":             []any{map[string]any{"tld": "co.uk", "price_yearly": 9.0, "price_yearly_discounted": nil}},
		"durations_months": []any{12, 24, 36},
	})
	accTest(t, tfresource.TestStep{
		Config: d.config(map[string]string{"name": `"shop.co.uk"`, "duration_months": "24"}),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(domainAddr, "name", "shop.co.uk"),
			checkMock(func() error {
				it := lastOrderItem(t, d.api)
				if it["sld"] != "shop" || it["tld"] != "co.uk" || it["client_reference"] != "shop.co.uk" || it["duration_months"] != float64(24) {
					return fmt.Errorf("item = %v", it)
				}
				return nil
			}),
		),
	})
}

// Config errors are caught at plan time: nothing is quoted or ordered.
func TestAccDomain_ConfigValidation(t *testing.T) {
	d := newDomainTest(t)
	cases := map[string]struct {
		set map[string]string
		err string
	}{
		"transfer":         {map[string]string{"action": `"transfer"`}, `(?i)transfers.*not available.*customer panel`},
		"no contact_id":    {map[string]string{"contact_id": "-"}, `contact_id`},
		"bad action":       {map[string]string{"action": `"renew"`}, `action`},
		"bad duration":     {map[string]string{"duration_months": "6"}, `duration_months`},
		"upper case":       {map[string]string{"name": `"Example.de"`}, `name`},
		"single label":     {map[string]string{"name": `"example"`}, `name`},
		"trailing dot":     {map[string]string{"name": `"example.de."`}, `name`},
		"leading hyphen":   {map[string]string{"name": `"-example.de"`}, `name`},
		"too long":         {map[string]string{"name": `"` + strings.Repeat("a", 62) + `.de"`}, `name`},
		"one nameserver":   {map[string]string{"nameservers": `["ns1.example.com"]`}, `nameservers`},
		"nameserver ip":    {map[string]string{"nameservers": `["ns1.example.com", "192.0.2.1"]`}, `nameservers`},
		"nameserver upper": {map[string]string{"nameservers": `["NS1.example.com", "ns2.example.com"]`}, `nameservers`},
		"nameserver dot":   {map[string]string{"nameservers": `["ns1.example.com.", "ns2.example.com"]`}, `nameservers`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			accTest(t, tfresource.TestStep{Config: d.config(tc.set), PlanOnly: true, ExpectError: re("(?s)" + tc.err)})
		})
	}
	if n := d.api.CallCount(http.MethodPost, "/orders/quote"); n != 0 || d.api.WriteCount() != 0 {
		t.Errorf("quotes = %d, writes = %d: config errors must stop at plan time", n, d.api.WriteCount())
	}
}

func TestAccDomain_Unavailable(t *testing.T) {
	d := newDomainTest(t)
	d.api.SetDomainUnavailable("example.de")
	accTest(t, tfresource.TestStep{
		Config:      d.config(nil),
		ExpectError: re(`(?s)example\.de cannot be registered.*terraform import.*DOMAIN_UNAVAILABLE`),
	})
	if d.api.ChargeCount() != 0 || len(d.api.ResourceIDs(testapi.ResourceDomain)) != 0 || d.api.CallCount(http.MethodPost, "/orders") != 0 {
		t.Errorf("charges = %d, domains = %v", d.api.ChargeCount(), d.api.ResourceIDs(testapi.ResourceDomain))
	}
}

// A domain registered in the panel is taken at the registrar and carries no
// API order reference: ordering it is DOMAIN_UNAVAILABLE with the import hint,
// nothing is charged.
func TestAccDomain_PanelDomainNeedsImport(t *testing.T) {
	d := newDomainTest(t)
	existing := d.api.SeedDomain(map[string]any{"sld": "example", "tld": "de"})
	accTest(t, tfresource.TestStep{
		Config:      d.config(nil),
		ExpectError: re(`(?s)example\.de cannot be registered.*already in this account.*terraform import.*DOMAIN_UNAVAILABLE`),
	})
	if d.api.ChargeCount() != 0 || d.api.CallCount(http.MethodPost, "/orders") != 0 || len(d.api.ResourceIDs(testapi.ResourceDomain)) != 1 {
		t.Errorf("charges = %d, domains = %v (want only %d)", d.api.ChargeCount(), d.api.ResourceIDs(testapi.ResourceDomain), existing)
	}
}

// Review Focus 3: a purchase argument changed → plan error, no API writes.
func TestAccDomain_PurchaseFieldChangeIsAPlanError(t *testing.T) {
	d := newDomainTest(t)
	var writes int
	accTest(t,
		tfresource.TestStep{
			Config: d.config(nil),
			Check:  checkMock(func() error { writes = d.api.WriteCount(); return nil }),
		},
		tfresource.TestStep{
			Config:      d.config(map[string]string{"name": `"example.com"`}),
			ExpectError: re(`(?s)Purchase arguments cannot be changed.*name.*identifies the purchased domain`),
		},
		tfresource.TestStep{
			Config:      d.config(map[string]string{"duration_months": "24"}),
			ExpectError: re(`(?s)Purchase arguments cannot be changed.*duration_months`),
		},
		tfresource.TestStep{
			PreConfig: func() {
				if d.api.WriteCount() != writes || d.api.ChargeCount() != 1 {
					t.Errorf("writes %d → %d, charges %d: the plan errors must not write", writes, d.api.WriteCount(), d.api.ChargeCount())
				}
			},
			Config:   d.config(nil),
			PlanOnly: true,
		},
	)
}

func TestAccDomain_NameserversAndAutoRenewUpdate(t *testing.T) {
	d := newDomainTest(t)
	var id string
	var orders int
	accTest(t,
		tfresource.TestStep{
			Config: d.config(map[string]string{"nameservers": `["ns1.example.net", "ns2.example.net"]`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(domainAddr, "nameservers.#", "2"),
				tfresource.TestCheckResourceAttrWith(domainAddr, "id", func(v string) error {
					id = v
					orders = d.api.CallCount(http.MethodPost, "/orders")
					return nil
				}),
				checkMock(func() error {
					if got := fmt.Sprint(domainAttr(d.api, domainID(t, d.api), "nameservers")); got != "[ns1.example.net ns2.example.net]" {
						return fmt.Errorf("nameservers = %v", got)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: d.config(map[string]string{"nameservers": `["ns1.example.org", "ns2.example.org", "ns3.example.org"]`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrPtr(domainAddr, "id", &id),
				tfresource.TestCheckResourceAttr(domainAddr, "nameservers.2", "ns3.example.org"),
				wantCharges(d.api, 1),
				checkMock(func() error {
					if got := fmt.Sprint(domainAttr(d.api, domainID(t, d.api), "nameservers")); got != "[ns1.example.org ns2.example.org ns3.example.org]" {
						return fmt.Errorf("nameservers = %v", got)
					}
					if n := d.api.CallCount(http.MethodPost, "/orders"); n != orders {
						return fmt.Errorf("POST /orders %d → %d: a nameserver change must not order", orders, n)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: d.config(map[string]string{"nameservers": `["ns1.example.org", "ns2.example.org", "ns3.example.org"]`, "auto_renew": "false"}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(domainAddr, "auto_renew", "false"),
				checkMock(func() error {
					if got := domainAttr(d.api, domainID(t, d.api), "auto_renew"); got != false {
						return fmt.Errorf("auto_renew = %v", got)
					}
					return nil
				}),
			),
		},
		// The registrar did not confirm: the error says the change may be underway.
		tfresource.TestStep{
			PreConfig: func() {
				d.api.InjectResponse(http.MethodPut, "/domain/{id}/nameservers", 1, testapi.Fail(http.StatusBadGateway,
					"NAMESERVER_STATE_UNKNOWN", "Status unbekannt.", map[string]any{"state_unknown": true}))
			},
			Config:      d.config(map[string]string{"nameservers": `["ns1.example.com", "ns2.example.com"]`, "auto_renew": "true"}),
			ExpectError: re(`(?s)may already be applied.*apply again`),
		},
		tfresource.TestStep{
			Config: d.config(map[string]string{"nameservers": `["ns1.example.com", "ns2.example.com"]`, "auto_renew": "true",
				"max_price": "50", "promo_code": `"LATER"`, "create_timeout": `"45m"`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(domainAddr, "auto_renew", "true"),
				tfresource.TestCheckResourceAttr(domainAddr, "nameservers.#", "2"),
				wantCharges(d.api, 1),
			),
		},
	)
	if n := d.api.CallCount(http.MethodPut, "/domain/{id}/nameservers"); n != 4 {
		t.Errorf("nameserver PUTs = %d, want 4 (create, change, failed, retry)", n)
	}
}

// Nameservers changed in the panel show as drift and are set back.
func TestAccDomain_NameserverDrift(t *testing.T) {
	d := newDomainTest(t)
	cfg := d.config(map[string]string{"nameservers": `["ns1.example.net", "ns2.example.net"]`})
	accTest(t,
		tfresource.TestStep{Config: cfg},
		tfresource.TestStep{
			PreConfig: func() {
				d.api.UpdateResource(testapi.ResourceDomain, domainID(t, d.api), func(a map[string]any) {
					a["nameservers"] = []string{"ns1.other.net", "ns2.other.net"}
				})
			},
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		tfresource.TestStep{
			Config: cfg,
			Check: checkMock(func() error {
				if got := fmt.Sprint(domainAttr(d.api, domainID(t, d.api), "nameservers")); got != "[ns1.example.net ns2.example.net]" {
					return fmt.Errorf("nameservers = %v", got)
				}
				return nil
			}),
		},
	)
}

// The registrar did not answer and the API fell back to its local ns1/ns2
// (source "local"): that is not drift, the state keeps its value.
func TestAccDomain_LocalNameserversAreNoDrift(t *testing.T) {
	d := newDomainTest(t)
	cfg := d.config(map[string]string{"nameservers": `["ns1.example.net", "ns2.example.net", "ns3.example.net"]`})
	accTest(t,
		tfresource.TestStep{Config: cfg},
		tfresource.TestStep{
			PreConfig: func() {
				d.api.UpdateResource(testapi.ResourceDomain, domainID(t, d.api), func(a map[string]any) { a["nameserver_source"] = "local" })
			},
			Config:   cfg,
			PlanOnly: true,
			Check:    tfresource.TestCheckResourceAttr(domainAddr, "nameservers.#", "3"),
		},
	)
}

// Destroy cancels: auto-renew off, the domain stays registered until expires_at.
func TestAccDomain_DestroyCancels(t *testing.T) {
	d := newDomainTest(t)
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    []tfresource.TestStep{{Config: d.config(nil)}},
		CheckDestroy: func(*terraform.State) error {
			id := domainID(t, d.api)
			if got := domainAttr(d.api, id, "auto_renew"); got != false {
				return fmt.Errorf("auto_renew = %v after destroy, want false", got)
			}
			for _, r := range d.api.Requests() {
				if r.Method == http.MethodDelete {
					return fmt.Errorf("destroy must not delete: %s", r.Path)
				}
			}
			return nil
		},
	})
}

// Review Focus 2: destroy, then apply the same configuration before the
// term ends → replayed, taken over, auto-renew back on; nothing bought twice.
func TestAccDomain_DestroyThenApplyAdopts(t *testing.T) {
	d := newDomainTest(t)
	var id string
	accTest(t,
		tfresource.TestStep{
			Config: d.config(nil),
			Check: tfresource.TestCheckResourceAttrWith(domainAddr, "id", func(v string) error {
				id = v
				return nil
			}),
		},
		tfresource.TestStep{Config: d.config(nil), Destroy: true},
		tfresource.TestStep{
			PreConfig: func() {
				if domainAttr(d.api, domainID(t, d.api), "auto_renew") != false {
					t.Errorf("destroy did not switch auto-renew off")
				}
				backdateOrders(d.api, time.Hour)
			},
			Config: d.config(nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrPtr(domainAddr, "id", &id),
				tfresource.TestCheckResourceAttr(domainAddr, "auto_renew", "true"),
				wantCharges(d.api, 1),
				checkMock(func() error {
					if got := domainAttr(d.api, domainID(t, d.api), "auto_renew"); got != true {
						return fmt.Errorf("auto_renew = %v, want true again", got)
					}
					return nil
				}),
			),
		},
	)
}

// Review Focus 1: the apply is aborted after the charge (timeout while the
// registration runs); the next apply does not buy again.
func TestAccDomain_AbortedApplyIsNotBoughtTwice(t *testing.T) {
	d := newDomainTest(t)
	accTest(t,
		tfresource.TestStep{
			PreConfig:   func() { d.api.SetProvisioningPolls(1_000_000) },
			Config:      d.config(map[string]string{"create_timeout": `"1s"`}),
			ExpectError: re(`(?s)already paid.*will not be bought twice`),
		},
		tfresource.TestStep{
			PreConfig: func() {
				if d.api.ChargeCount() != 1 {
					t.Fatalf("charges = %d after the aborted apply", d.api.ChargeCount())
				}
				for _, o := range d.api.OrderIDs() {
					d.api.SetOrderItemStatus(o, 0, client.StatusActive)
				}
				backdateOrders(d.api, 10*time.Minute)
			},
			Config: d.config(nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(domainAddr, "status", "active"),
				tfresource.TestCheckResourceAttrWith(domainAddr, "id", func(v string) error {
					if v != strconv.FormatInt(domainID(t, d.api), 10) {
						return fmt.Errorf("id = %s, want the paid domain", v)
					}
					return nil
				}),
				wantCharges(d.api, 1),
			),
		},
	)
}

// A live domain carries the name as client reference (CLIENT_REFERENCE_IN_USE):
// taken over, auto-renew switched on, nothing charged.
func TestAccDomain_AdoptsLiveDomain(t *testing.T) {
	d := newDomainTest(t)
	existing := d.api.SeedDomain(map[string]any{"client_reference": "example.de", "auto_renew": false})
	accTest(t, tfresource.TestStep{
		Config: d.config(nil),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(domainAddr, "id", strconv.FormatInt(existing, 10)),
			tfresource.TestCheckResourceAttr(domainAddr, "auto_renew", "true"),
			wantCharges(d.api, 0),
			checkMock(func() error {
				if got := domainAttr(d.api, existing, "auto_renew"); got != true {
					return fmt.Errorf("auto_renew = %v", got)
				}
				return nil
			}),
		),
	})
}

// Review Focus 5: deleted outside Terraform → removed from the state.
func TestAccDomain_DeletedIsRemovedFromState(t *testing.T) {
	d := newDomainTest(t)
	accTest(t,
		tfresource.TestStep{Config: d.config(nil)},
		tfresource.TestStep{
			PreConfig:          func() { d.api.DeleteResource(testapi.ResourceDomain, domainID(t, d.api)) },
			RefreshState:       true,
			ExpectNonEmptyPlan: true,
			Check: func(s *terraform.State) error {
				if rs := s.RootModule().Resources[domainAddr]; rs != nil {
					return fmt.Errorf("still in state: %v", rs.Primary.ID)
				}
				return nil
			},
		},
		tfresource.TestStep{Config: d.config(nil), PlanOnly: true, ExpectNonEmptyPlan: true},
	)
}

// Review Focus 5: expired but still listed → stays in the state as
// "expired", the next apply buys nothing.
func TestAccDomain_ExpiredStaysInStateWithoutPurchase(t *testing.T) {
	d := newDomainTest(t)
	accTest(t,
		tfresource.TestStep{Config: d.config(nil)},
		tfresource.TestStep{
			PreConfig: func() {
				d.api.UpdateResource(testapi.ResourceDomain, domainID(t, d.api), func(a map[string]any) {
					a["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
				})
			},
			Config: d.config(nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(domainAddr, "status", "expired"),
				wantCharges(d.api, 1),
				checkMock(func() error {
					if n := d.api.CallCount(http.MethodPost, "/orders"); n != 1 {
						return fmt.Errorf("POST /orders = %d, want 1", n)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{Config: d.config(nil), PlanOnly: true},
	)
}

func TestAccDomain_InsufficientBalance(t *testing.T) {
	d := newDomainTest(t)
	d.api.SetBalance(1.5)
	accTest(t, tfresource.TestStep{
		Config:      d.config(nil),
		ExpectError: re(`(?s)10\.50 EUR short.*Top up the balance`),
	})
	if d.api.ChargeCount() != 0 || len(d.api.ResourceIDs(testapi.ResourceDomain)) != 0 {
		t.Errorf("charges = %d, domains = %v", d.api.ChargeCount(), d.api.ResourceIDs(testapi.ResourceDomain))
	}
}

func TestAccDomain_MaxPrice(t *testing.T) {
	d := newDomainTest(t)
	accTest(t, tfresource.TestStep{
		Config:      d.config(map[string]string{"max_price": "11.99"}),
		ExpectError: re(`(?s)Price above max_price.*12\.00`),
	})
	if d.api.CallCount(http.MethodPost, "/orders") != 0 {
		t.Errorf("max_price exceeded must not order")
	}
}

func TestAccDomain_Import(t *testing.T) {
	d := newDomainTest(t)
	accTest(t,
		tfresource.TestStep{Config: d.config(nil)},
		tfresource.TestStep{
			ResourceName:      domainAddr,
			ImportState:       true,
			ImportStateVerify: true,
			// Not readable from the API (only used for the purchase, or write-only).
			ImportStateVerifyIgnore: []string{"duration_months", "action", "authcode", "contact_id", "max_price", "promo_code", "create_timeout"},
		},
	)
}

// A domain registered in the panel is imported; the first apply buys nothing.
func TestAccDomain_ImportExisting(t *testing.T) {
	d := newDomainTest(t)
	existing := d.api.SeedDomain(map[string]any{"sld": "panel", "tld": "de"})
	id := strconv.FormatInt(existing, 10)
	cfg := d.config(map[string]string{"name": `"panel.de"`})
	accTest(t,
		initStep(d.api),
		tfresource.TestStep{
			Config:             importConfig(cfg),
			ResourceName:       domainAddr,
			ImportState:        true,
			ImportStateId:      id,
			ImportStatePersist: true,
			ImportStateCheck: func(states []*terraform.InstanceState) error {
				if len(states) != 1 || states[0].Attributes["name"] != "panel.de" || states[0].Attributes["auto_renew"] != "true" ||
					states[0].Attributes["nameservers.#"] != "4" {
					return fmt.Errorf("imported = %v", states)
				}
				return nil
			},
		},
		tfresource.TestStep{
			Config: cfg,
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(domainAddr, "id", id),
				wantCharges(d.api, 0),
			),
		},
		tfresource.TestStep{
			Config:        importConfig(cfg),
			ResourceName:  domainAddr,
			ImportState:   true,
			ImportStateId: "panel.de",
			ExpectError:   re("numeric"),
		},
	)
}

// ---- direct calls (warnings are not visible through terraform-plugin-testing) ----

func domainSchema(t *testing.T) resource.SchemaResponse {
	t.Helper()
	var sch resource.SchemaResponse
	NewDomainResource().Schema(context.Background(), resource.SchemaRequest{}, &sch)
	return sch
}

func (d domainTest) values() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"name": str("example.de"), "contact_id": num(float64(d.contact)), "duration_months": num(12), "action": str("register"),
		"auto_renew": tftypes.NewValue(tftypes.Bool, true), "create_timeout": str("20s"),
	}
}

func (d domainTest) resource() *domainResource {
	res := NewDomainResource().(*domainResource) //nolint:forcetypeassert // test
	res.client = client.New(d.api.URL, d.api.Token, client.WithRetryBackoff(time.Millisecond), client.WithPollInterval(10*time.Millisecond))
	return res
}

func TestDomain_Schema(t *testing.T) {
	sch := domainSchema(t)
	// Transfers are not available through the API: no auth code to take.
	if _, ok := sch.Schema.Attributes["authcode"]; ok {
		t.Errorf("authcode must not exist while transfers are refused")
	}
	for _, f := range []string{"id", "status", "expires_at"} {
		if a := sch.Schema.Attributes[f]; a == nil || !a.IsComputed() || a.IsOptional() {
			t.Errorf("%s must be computed only", f)
		}
	}
}

func TestDomain_AdoptionWarns(t *testing.T) {
	d := newDomainTest(t)
	existing := d.api.SeedDomain(map[string]any{"client_reference": "example.de", "auto_renew": false})
	sch := domainSchema(t)
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch.Schema, Raw: tfValue(t, sch, d.values(), false)},
		Config: tfsdk.Config{Schema: sch.Schema, Raw: tfValue(t, sch, d.values(), true)},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}}
	d.resource().Create(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	w := resp.Diagnostics.Warnings()
	if len(w) != 1 || w[0].Summary() != "Existing domain taken over" || !strings.Contains(w[0].Detail(), strconv.FormatInt(existing, 10)) ||
		!strings.Contains(w[0].Detail(), "Auto-renew is on") {
		t.Errorf("want the take-over warning, got %v", resp.Diagnostics)
	}
}

func (d domainTest) readDirect(t *testing.T, id int64) resource.ReadResponse {
	t.Helper()
	sch := domainSchema(t)
	vals := d.values()
	vals["id"] = str(strconv.FormatInt(id, 10))
	state := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}
	resp := resource.ReadResponse{State: state}
	d.resource().Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	return resp
}

func TestDomain_ReadExpiredWarnsAndKeeps(t *testing.T) {
	d := newDomainTest(t)
	id := d.api.SeedDomain(map[string]any{"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	resp := d.readDirect(t, id)
	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("want the domain kept, got %v", resp.Diagnostics)
	}
	if got := stateString(t, resp, "status"); got != "expired" {
		t.Errorf("status = %q", got)
	}
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "expired on") ||
		!strings.Contains(w[0].Detail(), "no longer renews it") {
		t.Errorf("want an expiry warning, got %v", resp.Diagnostics)
	}
}

func TestDomain_ReadLocalNameserversKeepState(t *testing.T) {
	d := newDomainTest(t)
	id := d.api.SeedDomain(map[string]any{"nameserver_source": "local"})
	sch := domainSchema(t)
	vals := d.values()
	vals["id"] = str(strconv.FormatInt(id, 10))
	vals["nameservers"] = tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("a.example.net"), str("b.example.net"), str("c.example.net")})
	state := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}
	resp := resource.ReadResponse{State: state}
	d.resource().Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	var ns []string
	resp.State.GetAttribute(context.Background(), path.Root("nameservers"), &ns)
	if resp.Diagnostics.HasError() || fmt.Sprint(ns) != "[a.example.net b.example.net c.example.net]" {
		t.Errorf("nameservers = %v, want the state kept (%v)", ns, resp.Diagnostics)
	}
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "registrar") {
		t.Errorf("want a warning about the registrar, got %v", resp.Diagnostics)
	}
}

func TestDomain_ReadGoneRemoves(t *testing.T) {
	d := newDomainTest(t)
	resp := d.readDirect(t, 424242)
	if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Errorf("want removed from state, got %v", resp.Diagnostics)
	}
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Summary(), "no longer exists") {
		t.Errorf("want a warning, got %v", resp.Diagnostics)
	}
}

// show finds the domain, but it has no rental: an error, not "gone".
func TestDomain_ReadWithoutRentalIsAnError(t *testing.T) {
	d := newDomainTest(t)
	id := d.api.SeedDomain(map[string]any{"no_rental": true})
	resp := d.readDirect(t, id)
	if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() || !strings.Contains(errorText(resp.Diagnostics), "no term") {
		t.Errorf("want an error and the state kept, got %v", resp.Diagnostics)
	}
}

func TestDomain_DestroyWarnsWithExpiry(t *testing.T) {
	d := newDomainTest(t)
	id := d.api.SeedDomain(map[string]any{"expires_at": "2030-01-02T03:04:05Z"})
	sch := domainSchema(t)
	vals := d.values()
	vals["id"] = str(strconv.FormatInt(id, 10))
	var resp resource.DeleteResponse
	d.resource().Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics)
	}
	w := resp.Diagnostics.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].Summary(), "cancelled") || !strings.Contains(w[0].Detail(), "2030-01-02") ||
		!strings.Contains(w[0].Detail(), "no refund") {
		t.Errorf("want one cancel warning with the expiry date and 'no refund', got %v", resp.Diagnostics)
	}
	if domainAttr(d.api, id, "auto_renew") != false {
		t.Errorf("auto-renew not switched off")
	}
}

func TestDomain_DestroyOfGoneDomainSucceeds(t *testing.T) {
	d := newDomainTest(t)
	sch := domainSchema(t)
	vals := d.values()
	vals["id"] = str("424242")
	var resp resource.DeleteResponse
	d.resource().Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("delete of a gone domain must succeed: %v", resp.Diagnostics)
	}
}

func domainModifyPlan(t *testing.T, state, plan map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	sch := domainSchema(t)
	st := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, state, true)}
	pl := tfsdk.Plan{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}
	if plan != nil {
		pl.Raw = tfValue(t, sch, plan, true)
	}
	resp := resource.ModifyPlanResponse{Plan: pl}
	NewDomainResource().(*domainResource).ModifyPlan(context.Background(), //nolint:forcetypeassert // test
		resource.ModifyPlanRequest{State: st, Plan: pl, Config: tfsdk.Config{Schema: sch.Schema, Raw: pl.Raw}}, &resp)
	return resp
}

// contact_id is only used when buying: a change warns, it does not fail.
func TestDomain_ContactChangeWarnsAtPlan(t *testing.T) {
	d := domainTest{contact: 5}
	state := d.values()
	state["id"] = str("1001")
	plan := d.values()
	plan["id"] = str("1001")
	plan["contact_id"] = num(6)
	resp := domainModifyPlan(t, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("errors: %v", resp.Diagnostics)
	}
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "registrant") {
		t.Errorf("want the contact warning, got %v", resp.Diagnostics)
	}
}

func TestDomain_DestroyPlanWarns(t *testing.T) {
	d := domainTest{contact: 5}
	state := d.values()
	state["id"] = str("1001")
	state["expires_at"] = str("2030-01-02T03:04:05Z")
	resp := domainModifyPlan(t, state, nil)
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "2030-01-02") || !strings.Contains(w[0].Detail(), "no refund") {
		t.Errorf("want the destroy warning, got %v", resp.Diagnostics)
	}
}

// After an import the purchase arguments are unknown: the configuration is taken.
func TestDomain_PurchaseArgumentsAfterImportAreTaken(t *testing.T) {
	d := domainTest{contact: 5}
	state := map[string]tftypes.Value{"id": str("1001"), "name": str("example.de"), "auto_renew": tftypes.NewValue(tftypes.Bool, true)}
	plan := d.values()
	plan["id"] = str("1001")
	plan["duration_months"] = num(24)
	if resp := domainModifyPlan(t, state, plan); resp.Diagnostics.HasError() {
		t.Errorf("errors: %v", resp.Diagnostics)
	}
}

// ---- follow-ups: local nameservers on create, unfinished orders on adoption ----

func (d domainTest) createDirect(t *testing.T, set map[string]tftypes.Value) resource.CreateResponse {
	t.Helper()
	sch := domainSchema(t)
	vals := d.values()
	for k, v := range set {
		vals[k] = v
	}
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch.Schema, Raw: tfValue(t, sch, vals, false)},
		Config: tfsdk.Config{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}}
	d.resource().Create(context.Background(), req, &resp)
	return resp
}

func createdNameservers(t *testing.T, resp resource.CreateResponse) types.List {
	t.Helper()
	var ns types.List
	resp.State.GetAttribute(context.Background(), path.Root("nameservers"), &ns)
	return ns
}

func hasWarning(diags diag.Diagnostics, summary string) bool {
	for _, w := range diags.Warnings() {
		if w.Summary() == summary {
			return true
		}
	}
	return false
}

// The registrar does not answer (source "local") and no nameservers are
// configured: nothing is sent, the local ns1/ns2 are not stored, a warning says so.
func TestDomain_CreateLocalNameserversStayNull(t *testing.T) {
	d := newDomainTest(t)
	d.api.SetProvisioner("domain", func(_, attrs map[string]any) { attrs["nameserver_source"] = "local" })
	resp := d.createDirect(t, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if ns := createdNameservers(t, resp); !ns.IsNull() {
		t.Errorf("nameservers = %v, want null", ns)
	}
	if !hasWarning(resp.Diagnostics, "Nameservers not readable from the registrar") {
		t.Errorf("want the registrar warning, got %v", resp.Diagnostics)
	}
	if n := d.api.CallCount(http.MethodPut, "/domain/{id}/nameservers"); n != 0 {
		t.Errorf("PUT nameservers = %d, want 0", n)
	}
}

// Configured nameservers are not sent while the registrar cannot be read.
func TestDomain_CreateLocalNameserversConfiguredAreNotSent(t *testing.T) {
	d := newDomainTest(t)
	d.api.SetProvisioner("domain", func(_, attrs map[string]any) { attrs["nameserver_source"] = "local" })
	resp := d.createDirect(t, map[string]tftypes.Value{
		"nameservers": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("a.example.net"), str("b.example.net")}),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	var ns []string
	resp.State.GetAttribute(context.Background(), path.Root("nameservers"), &ns)
	if fmt.Sprint(ns) != "[a.example.net b.example.net]" {
		t.Errorf("nameservers = %v, want the configured list", ns)
	}
	if !hasWarning(resp.Diagnostics, "Nameservers not applied yet") {
		t.Errorf("want the not-applied warning, got %v", resp.Diagnostics)
	}
	if n := d.api.CallCount(http.MethodPut, "/domain/{id}/nameservers"); n != 0 {
		t.Errorf("PUT nameservers = %d, want 0", n)
	}
}

// A domain taken over by its reference whose earlier order is still
// provisioning is paid but not active: status says so and a warning appears.
func TestDomain_AdoptionOfUnfinishedOrderIsProvisioning(t *testing.T) {
	d := newDomainTest(t)
	existing := d.api.SeedDomain(map[string]any{"client_reference": "example.de"})
	d.api.SeedOrder("domain", testapi.ResourceDomain, existing, "example.de", "provisioning")
	resp := d.createDirect(t, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	var status types.String
	resp.State.GetAttribute(context.Background(), path.Root("status"), &status)
	if status.ValueString() != "provisioning" {
		t.Errorf("status = %q, want provisioning", status.ValueString())
	}
	if !hasWarning(resp.Diagnostics, "Domain paid but not yet active") || !hasWarning(resp.Diagnostics, "Existing domain taken over") {
		t.Errorf("want both warnings, got %v", resp.Diagnostics)
	}
}

// An adopted domain with an active order item (and one from the panel, which
// has no order at all) stays active and gets no extra warning.
func TestDomain_AdoptionOfActiveOrderStaysActive(t *testing.T) {
	for name, withOrder := range map[string]bool{"active item": true, "no order": false} {
		t.Run(name, func(t *testing.T) {
			d := newDomainTest(t)
			existing := d.api.SeedDomain(map[string]any{"client_reference": "example.de"})
			if withOrder {
				d.api.SeedOrder("domain", testapi.ResourceDomain, existing, "example.de", "active")
			}
			resp := d.createDirect(t, nil)
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			var status types.String
			resp.State.GetAttribute(context.Background(), path.Root("status"), &status)
			if status.ValueString() != "active" || hasWarning(resp.Diagnostics, "Domain paid but not yet active") {
				t.Errorf("status = %q, diags = %v", status.ValueString(), resp.Diagnostics)
			}
		})
	}
}

// Read keeps "provisioning" while the order item is not active, and moves on
// once it is.
func TestDomain_ReadKeepsProvisioningUntilActive(t *testing.T) {
	d := newDomainTest(t)
	id := d.api.SeedDomain(map[string]any{"client_reference": "example.de"})
	orderID := d.api.SeedOrder("domain", testapi.ResourceDomain, id, "example.de", "provisioning")

	read := func() resource.ReadResponse {
		sch := domainSchema(t)
		vals := d.values()
		vals["id"] = str(strconv.FormatInt(id, 10))
		vals["status"] = str("provisioning")
		state := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}
		resp := resource.ReadResponse{State: state}
		d.resource().Read(context.Background(), resource.ReadRequest{State: state}, &resp)
		return resp
	}
	resp := read()
	if got := stateString(t, resp, "status"); got != "provisioning" || !hasWarning(resp.Diagnostics, "Domain paid but not yet active") {
		t.Errorf("status = %q, diags = %v", got, resp.Diagnostics)
	}
	d.api.SetOrderItemStatus(orderID, 0, "active")
	resp = read()
	if got := stateString(t, resp, "status"); got != "active" || hasWarning(resp.Diagnostics, "Domain paid but not yet active") {
		t.Errorf("status = %q, diags = %v", got, resp.Diagnostics)
	}
	if stateString(t, resp, "expires_at") == "" {
		t.Errorf("expires_at must be read together with the status")
	}
}

// When the orders cannot be read, Read keeps the last known "provisioning"
// and warns, instead of taking "unknown" for "active".
func TestDomain_ReadKeepsProvisioningWhenOrdersUnreadable(t *testing.T) {
	d := newDomainTest(t)
	id := d.api.SeedDomain(map[string]any{"client_reference": "example.de"})
	d.api.SeedOrder("domain", testapi.ResourceDomain, id, "example.de", "provisioning")
	d.api.InjectResponse(http.MethodGet, "/orders", 10,
		testapi.Fail(http.StatusInternalServerError, "INTERNAL_ERROR", "Ein unerwarteter Fehler ist aufgetreten.", nil))

	sch := domainSchema(t)
	vals := d.values()
	vals["id"] = str(strconv.FormatInt(id, 10))
	vals["status"] = str("provisioning")
	state := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}
	resp := resource.ReadResponse{State: state}
	d.resource().Read(context.Background(), resource.ReadRequest{State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp, "status"); got != "provisioning" || !hasWarning(resp.Diagnostics, "Could not check the domain's order") {
		t.Errorf("status = %q, diags = %v", got, resp.Diagnostics)
	}
}

// Adopting a domain while the orders cannot be read warns that its status
// could not be checked.
func TestDomain_AdoptionWarnsWhenOrdersUnreadable(t *testing.T) {
	d := newDomainTest(t)
	existing := d.api.SeedDomain(map[string]any{"client_reference": "example.de"})
	d.api.SeedOrder("domain", testapi.ResourceDomain, existing, "example.de", "provisioning")
	d.api.InjectResponse(http.MethodGet, "/orders", 10,
		testapi.Fail(http.StatusInternalServerError, "INTERNAL_ERROR", "Ein unerwarteter Fehler ist aufgetreten.", nil))
	resp := d.createDirect(t, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !hasWarning(resp.Diagnostics, "Could not check the domain's order") {
		t.Errorf("want the warning, got %v", resp.Diagnostics)
	}
}
