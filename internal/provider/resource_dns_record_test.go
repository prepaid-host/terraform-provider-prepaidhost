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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

const dnsAddr = "prepaidhost_dns_record.rec"

type dnsTest struct {
	api    *testapi.Server
	domain int64
}

func newDNSTest(t *testing.T) dnsTest {
	t.Helper()
	api := testapi.New(t)
	return dnsTest{api: api, domain: api.SeedDomain(nil)}
}

// config renders one record; set overrides or adds arguments (raw HCL), "-" removes one.
func (d dnsTest) config(set map[string]string) string {
	args := map[string]string{
		"domain_id": strconv.FormatInt(d.domain, 10),
		"type":      `"A"`,
		"host":      `"www"`,
		"record":    `"192.0.2.1"`,
	}
	for k, v := range set {
		args[k] = v
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(providerConfig(d.api))
	b.WriteString("resource \"prepaidhost_dns_record\" \"rec\" {\n")
	for _, k := range keys {
		if args[k] != "-" {
			fmt.Fprintf(&b, "  %s = %s\n", k, args[k])
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// managed returns the zone without the apex NS records.
func (d dnsTest) managed() []map[string]any {
	var out []map[string]any
	for _, r := range d.api.DNSRecords(d.domain) {
		if r["type"] != "NS" {
			out = append(out, r)
		}
	}
	return out
}

func (d dnsTest) only(t *testing.T) map[string]any {
	t.Helper()
	recs := d.managed()
	if len(recs) != 1 {
		t.Fatalf("records = %v, want exactly one", recs)
	}
	return recs[0]
}

func TestAccDNSRecord_CRUDAndImport(t *testing.T) {
	d := newDNSTest(t)
	var id string
	accTest(t,
		tfresource.TestStep{
			Config: d.config(map[string]string{"ttl": "3600"}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrWith(dnsAddr, "id", func(v string) error {
					id = v
					if got := fmt.Sprint(d.only(t)["id"]); got != v {
						return fmt.Errorf("id = %s, record in the zone = %s", v, got)
					}
					return nil
				}),
				tfresource.TestCheckResourceAttr(dnsAddr, "ttl", "3600"),
				tfresource.TestCheckNoResourceAttr(dnsAddr, "priority"),
				checkMock(func() error {
					r := d.only(t)
					if r["type"] != "A" || r["host"] != "www" || r["record"] != "192.0.2.1" || r["ttl"] != int64(3600) {
						return fmt.Errorf("record = %v", r)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: d.config(map[string]string{"ttl": "3600", "record": `"192.0.2.2"`, "host": `"@"`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrPtr(dnsAddr, "id", &id),
				checkMock(func() error {
					r := d.only(t)
					if fmt.Sprint(r["id"]) != id || r["record"] != "192.0.2.2" || r["host"] != "@" {
						return fmt.Errorf("record = %v (want updated in place)", r)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			ResourceName:      dnsAddr,
			ImportState:       true,
			ImportStateVerify: true,
			ImportStateIdFunc: func(*terraform.State) (string, error) {
				return fmt.Sprintf("%d/%s", d.domain, id), nil
			},
		},
		// Leaving ttl out uses the default 300.
		tfresource.TestStep{
			Config: d.config(map[string]string{"record": `"192.0.2.2"`, "host": `"@"`}),
			Check:  tfresource.TestCheckResourceAttr(dnsAddr, "ttl", "300"),
		},
	)
	if n := d.api.CallCount(http.MethodPost, "/domain/{id}/dns"); n != 1 {
		t.Errorf("creates = %d, want 1", n)
	}
	if len(d.managed()) != 0 {
		t.Errorf("destroy left records: %v", d.managed())
	}
}

func TestAccDNSRecord_MXDefaultPriority(t *testing.T) {
	d := newDNSTest(t)
	cfg := d.config(map[string]string{"type": `"MX"`, "host": `"@"`, "record": `"mail.example.com"`})
	accTest(t,
		tfresource.TestStep{Config: cfg, Check: tfresource.TestCheckResourceAttr(dnsAddr, "priority", "10")},
		tfresource.TestStep{Config: cfg, PlanOnly: true},
		tfresource.TestStep{
			Config: d.config(map[string]string{"type": `"MX"`, "host": `"@"`, "record": `"mail.example.com"`, "priority": "20"}),
			Check: checkMock(func() error {
				if p := d.only(t)["priority"]; p != int64(20) {
					return fmt.Errorf("priority = %v", p)
				}
				return nil
			}),
		},
	)
}

func TestAccDNSRecord_SRV(t *testing.T) {
	d := newDNSTest(t)
	set := map[string]string{"type": `"SRV"`, "host": `"_sip._tcp"`, "record": `"sip.example.com"`, "priority": "5", "weight": "10", "port": "5060"}
	accTest(t,
		tfresource.TestStep{
			Config: d.config(set),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(dnsAddr, "port", "5060"),
				checkMock(func() error {
					r := d.only(t)
					if r["priority"] != int64(5) || r["weight"] != int64(10) || r["port"] != int64(5060) {
						return fmt.Errorf("record = %v", r)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: d.config(map[string]string{"type": `"SRV"`, "host": `"_sip._tcp"`, "record": `"sip.example.com"`, "priority": "5", "weight": "10", "port": "5061"}),
			Check: checkMock(func() error {
				if p := d.only(t)["port"]; p != int64(5061) {
					return fmt.Errorf("port = %v", p)
				}
				return nil
			}),
		},
	)
}

// The type cannot change in place (the API refuses it): the record is replaced.
func TestAccDNSRecord_TypeChangeReplaces(t *testing.T) {
	d := newDNSTest(t)
	var id string
	accTest(t,
		tfresource.TestStep{
			Config: d.config(nil),
			Check:  tfresource.TestCheckResourceAttrWith(dnsAddr, "id", func(v string) error { id = v; return nil }),
		},
		tfresource.TestStep{
			Config: d.config(map[string]string{"type": `"AAAA"`, "record": `"2001:db8::1"`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrWith(dnsAddr, "id", func(v string) error {
					if v == id {
						return fmt.Errorf("id unchanged: the record must be replaced")
					}
					return nil
				}),
				checkMock(func() error {
					if r := d.only(t); r["type"] != "AAAA" {
						return fmt.Errorf("record = %v", r)
					}
					return nil
				}),
			),
		},
	)
}

// Drift: a record deleted outside Terraform is removed from the state and
// planned again; a changed one is set back.
func TestAccDNSRecord_Drift(t *testing.T) {
	d := newDNSTest(t)
	recordID := func() int64 { return toInt64(d.only(t)["id"]) }
	accTest(t,
		tfresource.TestStep{Config: d.config(nil)},
		tfresource.TestStep{
			PreConfig:          func() { d.api.DeleteDNSRecord(d.domain, recordID()) },
			RefreshState:       true,
			ExpectNonEmptyPlan: true,
			Check: func(s *terraform.State) error {
				if rs := s.RootModule().Resources[dnsAddr]; rs != nil {
					return fmt.Errorf("still in state: %v", rs.Primary.ID)
				}
				return nil
			},
		},
		tfresource.TestStep{Config: d.config(nil)},
		tfresource.TestStep{
			PreConfig: func() {
				d.api.UpdateDNSRecord(d.domain, recordID(), func(r map[string]any) { r["record"] = "198.51.100.7" })
			},
			Config:             d.config(nil),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		tfresource.TestStep{
			Config: d.config(nil),
			Check: checkMock(func() error {
				if r := d.only(t); r["record"] != "192.0.2.1" {
					return fmt.Errorf("record = %v, want set back", r)
				}
				return nil
			}),
		},
	)
}

// The domain itself is gone: the record goes from the state too.
func TestAccDNSRecord_DomainGone(t *testing.T) {
	d := newDNSTest(t)
	accTest(t,
		tfresource.TestStep{Config: d.config(nil)},
		tfresource.TestStep{
			PreConfig:          func() { d.api.DeleteResource(testapi.ResourceDomain, d.domain) },
			RefreshState:       true,
			ExpectNonEmptyPlan: true,
			Check: func(s *terraform.State) error {
				if rs := s.RootModule().Resources[dnsAddr]; rs != nil {
					return fmt.Errorf("still in state: %v", rs.Primary.ID)
				}
				return nil
			},
		},
	)
}

// The API's validation rules for records, checked at plan time.
func TestAccDNSRecord_Validation(t *testing.T) {
	d := newDNSTest(t)
	cases := map[string]struct {
		set map[string]string
		err string
	}{
		"NS":                 {map[string]string{"type": `"NS"`}, `type`},
		"lower-case type":    {map[string]string{"type": `"a"`}, `type`},
		"ttl":                {map[string]string{"ttl": "301"}, `ttl`},
		"priority on A":      {map[string]string{"priority": "10"}, `priority.*MX.*SRV`},
		"priority too large": {map[string]string{"type": `"MX"`, "priority": "70000"}, `priority`},
		"weight on A":        {map[string]string{"weight": "1"}, `weight.*SRV`},
		"SRV without port":   {map[string]string{"type": `"SRV"`, "weight": "1"}, `port.*SRV`},
		"SRV port 0":         {map[string]string{"type": `"SRV"`, "weight": "1", "port": "0"}, `port`},
		"empty host":         {map[string]string{"host": `""`}, `host`},
		"host with space":    {map[string]string{"host": `" www"`}, `host`},
		"empty record":       {map[string]string{"record": `""`}, `record`},
		"no domain":          {map[string]string{"domain_id": "-"}, `domain_id`},
		"upper-case host":    {map[string]string{"host": `"WWW"`}, `host.*lower case`},
		"CNAME trailing dot": {map[string]string{"type": `"CNAME"`, "record": `"target.example.com."`}, `record.*trailing dot`},
		"MX trailing dot":    {map[string]string{"type": `"MX"`, "record": `"mail.example.com."`}, `record.*trailing dot`},
		"SRV trailing dot":   {map[string]string{"type": `"SRV"`, "record": `"sip.example.com."`, "weight": "1", "port": "5060"}, `record.*trailing dot`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			accTest(t, tfresource.TestStep{Config: d.config(tc.set), PlanOnly: true, ExpectError: re("(?s)" + tc.err)})
		})
	}
	if d.api.WriteCount() != 0 {
		t.Errorf("writes = %d, want 0", d.api.WriteCount())
	}
}

// TXT values are sent verbatim, a trailing dot included.
func TestAccDNSRecord_TXTVerbatim(t *testing.T) {
	d := newDNSTest(t)
	accTest(t, tfresource.TestStep{
		Config: d.config(map[string]string{"type": `"TXT"`, "host": `"_note"`, "record": `"Hello, World."`}),
		Check: checkMock(func() error {
			if r := d.only(t); r["record"] != "Hello, World." {
				return fmt.Errorf("record = %v", r)
			}
			return nil
		}),
	})
}

func TestAccDNSRecord_ImportErrors(t *testing.T) {
	d := newDNSTest(t)
	nsID := d.api.DNSRecords(d.domain)[0]["id"]
	cfg := importConfig(d.config(nil))
	accTest(t,
		initStep(d.api),
		tfresource.TestStep{Config: cfg, ResourceName: dnsAddr, ImportState: true, ImportStateId: "12", ExpectError: re("domain_id/record_id")},
		tfresource.TestStep{Config: cfg, ResourceName: dnsAddr, ImportState: true, ImportStateId: "a/b", ExpectError: re("domain_id/record_id")},
		tfresource.TestStep{
			Config: cfg, ResourceName: dnsAddr, ImportState: true,
			ImportStateId: fmt.Sprintf("%d/%v", d.domain, nsID), ExpectError: re("(?s)NS records.*cannot be managed"),
		},
		tfresource.TestStep{
			Config: cfg, ResourceName: dnsAddr, ImportState: true,
			ImportStateId: fmt.Sprintf("%d/999999", d.domain), ExpectError: re("(?s)Cannot import non-existent remote object"),
		},
	)
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
	return n
}

// ---- direct calls ----

func (d dnsTest) resource() *dnsRecordResource {
	res := NewDNSRecordResource().(*dnsRecordResource) //nolint:forcetypeassert // test
	res.client = client.New(d.api.URL, d.api.Token, client.WithRetryBackoff(time.Millisecond), client.WithPollInterval(10*time.Millisecond))
	return res
}

// A record already deleted (the API answers 422 for an unknown id): destroy succeeds.
func TestDNSRecord_DeleteOfGoneRecordSucceeds(t *testing.T) {
	d := newDNSTest(t)
	var sch resource.SchemaResponse
	NewDNSRecordResource().Schema(context.Background(), resource.SchemaRequest{}, &sch)
	vals := map[string]tftypes.Value{
		"id": str("999999"), "domain_id": num(float64(d.domain)), "type": str("A"), "host": str("www"), "record": str("192.0.2.1"), "ttl": num(300),
	}
	var resp resource.DeleteResponse
	d.resource().Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("delete of a gone record must succeed: %v", resp.Diagnostics)
	}

	// Any other refusal is an error.
	d.api.InjectResponse(http.MethodDelete, "/domain/{id}/dns/{recordId}", 1, testapi.Fail(http.StatusBadGateway, "DNS_PROVIDER_UNAVAILABLE", "down", nil))
	vals["id"] = str(fmt.Sprint(d.api.DNSRecords(d.domain)[0]["id"]))
	resp = resource.DeleteResponse{}
	d.resource().Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Errorf("a provider outage must fail the delete")
	}
}

// A refresh of many resources runs into the API's general rate limit (here
// scaled down to 10 requests per second) and recovers by waiting for
// Retry-After instead of failing.
func TestAccDNSRecord_RefreshUnderRateLimit(t *testing.T) {
	d := newDNSTest(t)
	cfg := providerConfig(d.api) + fmt.Sprintf(`
resource "prepaidhost_dns_record" "many" {
  count     = 15
  domain_id = %d
  type      = "A"
  host      = "host${count.index}"
  record    = "192.0.2.${count.index + 1}"
}
`, d.domain)
	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() { d.api.Throttle(10, time.Second) },
				Config:    cfg,
				Check: func(*terraform.State) error {
					if d.api.ThrottledCount() == 0 {
						return fmt.Errorf("the limit never refused a request; the test proves nothing")
					}
					return nil
				},
			},
			{RefreshState: true},
			// Writes are never retried on 429: lift the limit for the destroy.
			{PreConfig: func() { d.api.Throttle(0, 0) }, Config: cfg},
		},
	})
}
