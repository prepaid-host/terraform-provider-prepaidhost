// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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

const vserverAddr = "prepaidhost_vserver.web"

// vserverArgs are the arguments of the default (custom) test vServer, in order.
var vserverArgs = [][2]string{
	{"hostname", `"web1"`},
	{"cores", "2"},
	{"memory", "2048"},
	{"disk", "40"},
	{"addresses", "1"},
	{"backups", "1"},
	{"traffic", `"unlimited"`},
	{"duration_days", "30"},
	{"template_id", "11"},
	{"password", `"Sup3r-Secret!"`},
	{"ssh_key_ids", "[5, 3]"},
	{"create_timeout", `"20s"`},
}

// vserverConfig renders the test vServer; set overrides or adds arguments
// (raw HCL), "-" removes one.
func vserverConfig(api *testapi.Server, set map[string]string) string {
	var b strings.Builder
	b.WriteString(providerConfig(api))
	b.WriteString("resource \"prepaidhost_vserver\" \"web\" {\n")
	seen := map[string]bool{}
	for _, kv := range vserverArgs {
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

// packetConfig is the test vServer as root packet 2 ("Plus").
func packetConfig(api *testapi.Server, set map[string]string) string {
	cfg := map[string]string{
		"cores": "-", "memory": "-", "disk": "-", "addresses": "-", "backups": "-", "traffic": "-", "duration_days": "-",
		"packet_id": "2", "duration_months": "1",
	}
	for k, v := range set {
		cfg[k] = v
	}
	return vserverConfig(api, cfg)
}

func vserverAPI(t *testing.T) *testapi.Server {
	t.Helper()
	api := testapi.New(t)
	api.SetBalance(100)
	return api
}

// serverID returns the id of the only rootserver in the mock.
func serverID(t *testing.T, api *testapi.Server) int64 {
	t.Helper()
	ids := api.ResourceIDs(testapi.ResourceRootserver)
	if len(ids) != 1 {
		t.Fatalf("rootservers = %v, want exactly one", ids)
	}
	return ids[0]
}

func serverAttr(api *testapi.Server, id int64, key string) any {
	r, _ := api.Resource(testapi.ResourceRootserver, id)
	return r[key]
}

func checkMock(fn func() error) tfresource.TestCheckFunc {
	return func(*terraform.State) error { return fn() }
}

func wantCharges(api *testapi.Server, n int) tfresource.TestCheckFunc {
	return checkMock(func() error {
		if got := api.ChargeCount(); got != n {
			return fmt.Errorf("charges = %d, want %d", got, n)
		}
		return nil
	})
}

func lastOrderItem(t *testing.T, api *testapi.Server) map[string]any {
	t.Helper()
	body := lastOrderBody(t, api)
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	it, _ := items[0].(map[string]any)
	return it
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAccVserver_CreateCustom(t *testing.T) {
	api := vserverAPI(t)
	accTest(t, tfresource.TestStep{
		Config: vserverConfig(api, nil),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttrSet(vserverAddr, "id"),
			tfresource.TestCheckResourceAttr(vserverAddr, "status", "active"),
			tfresource.TestCheckResourceAttr(vserverAddr, "auto_renew", "true"),
			tfresource.TestCheckResourceAttr(vserverAddr, "ipv4_addresses.#", "1"),
			tfresource.TestCheckResourceAttrWith(vserverAddr, "ipv4_addresses.0", func(v string) error {
				if !strings.HasPrefix(v, "203.0.113.") {
					return fmt.Errorf("ipv4 = %q", v)
				}
				return nil
			}),
			tfresource.TestCheckResourceAttrWith(vserverAddr, "expires_at", func(v string) error {
				exp, err := time.Parse(time.RFC3339, v)
				if err != nil || exp.Before(time.Now().AddDate(0, 0, 27)) {
					return fmt.Errorf("expires_at = %q (%v)", v, err)
				}
				return nil
			}),
			// Write-only: never in the state.
			tfresource.TestCheckNoResourceAttr(vserverAddr, "password"),
			wantCharges(api, 1),
			checkMock(func() error {
				it := lastOrderItem(t, api)
				want := []string{"addresses", "backups", "client_reference", "cores", "disk", "duration_days", "hostname",
					"memory", "password", "ssh_key_ids", "template_id", "traffic", "type"}
				if got := keysOf(it); fmt.Sprint(got) != fmt.Sprint(want) {
					return fmt.Errorf("item fields = %v, want %v (exactly the item fields the API evaluates for a rootserver)", got, want)
				}
				if it["type"] != "rootserver" || it["client_reference"] != "web1" || it["hostname"] != "web1" ||
					it["password"] != "Sup3r-Secret!" || fmt.Sprint(it["ssh_key_ids"]) != "[3 5]" {
					return fmt.Errorf("item = %v", it)
				}
				return nil
			}),
		),
	})
}

func TestAccVserver_CreatePacket(t *testing.T) {
	api := vserverAPI(t)
	accTest(t, tfresource.TestStep{
		Config: packetConfig(api, map[string]string{"packet_promo_code": `"SPRING"`, "promo_code": `"ORDER10"`, "password": "-", "ssh_key_ids": "-"}),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(vserverAddr, "packet_id", "2"),
			tfresource.TestCheckNoResourceAttr(vserverAddr, "cores"),
			tfresource.TestCheckResourceAttr(vserverAddr, "status", "active"),
			wantCharges(api, 1),
			checkMock(func() error {
				body := lastOrderBody(t, api)
				if body["promo_code"] != "ORDER10" {
					return fmt.Errorf("order promo_code = %v", body["promo_code"])
				}
				it := lastOrderItem(t, api)
				want := []string{"client_reference", "duration_months", "hostname", "packet_id", "promo_code", "template_id", "type"}
				if got := keysOf(it); fmt.Sprint(got) != fmt.Sprint(want) {
					return fmt.Errorf("item fields = %v, want %v", got, want)
				}
				if it["type"] != "rootpacket" || it["promo_code"] != "SPRING" {
					return fmt.Errorf("item = %v", it)
				}
				return nil
			}),
		),
	}, tfresource.TestStep{
		// Read must not turn the packet's specs into custom arguments.
		Config:   packetConfig(api, map[string]string{"packet_promo_code": `"SPRING"`, "promo_code": `"ORDER10"`, "password": "-", "ssh_key_ids": "-"}),
		PlanOnly: true,
	})
}

func TestAccVserver_PacketAndCustomAreExclusive(t *testing.T) {
	api := vserverAPI(t)
	cases := map[string]struct {
		cfg string
		err string
	}{
		"both":                 {vserverConfig(api, map[string]string{"packet_id": "2", "duration_months": "1"}), "cores|packet_id"},
		"neither":              {packetConfig(api, map[string]string{"packet_id": "-", "duration_months": "-"}), "packet_id|cores"},
		"packet w/o months":    {packetConfig(api, map[string]string{"duration_months": "-"}), "duration_months"},
		"custom w/o memory":    {vserverConfig(api, map[string]string{"memory": "-"}), "memory"},
		"months with custom":   {vserverConfig(api, map[string]string{"duration_months": "1"}), "packet_id"},
		"packet promo, custom": {vserverConfig(api, map[string]string{"packet_promo_code": `"X"`}), "packet_id"},
		"bad duration_days":    {vserverConfig(api, map[string]string{"duration_days": "31"}), "duration_days"},
		"bad hostname":         {vserverConfig(api, map[string]string{"hostname": `"web_1.example"`}), "hostname"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			accTest(t, tfresource.TestStep{Config: tc.cfg, PlanOnly: true, ExpectError: re("(?s)" + tc.err)})
		})
	}
	if api.WriteCount() != 0 {
		t.Errorf("writes = %d, want 0", api.WriteCount())
	}
}

func TestVserver_PasswordIsWriteOnlyAndSensitive(t *testing.T) {
	var resp resource.SchemaResponse
	NewVserverResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	pw := resp.Schema.Attributes["password"]
	if pw == nil || !pw.IsWriteOnly() || !pw.IsSensitive() || !pw.IsOptional() {
		t.Errorf("password must be optional, write-only and sensitive: %+v", pw)
	}
	for _, f := range []string{"id", "ipv4_addresses", "status", "expires_at"} {
		if a := resp.Schema.Attributes[f]; a == nil || !a.IsComputed() || a.IsOptional() {
			t.Errorf("%s must be computed only", f)
		}
	}
}

// Review Focus 3: a purchase field changed → plan error, no API writes.
func TestAccVserver_PurchaseFieldChangeIsAPlanError(t *testing.T) {
	api := vserverAPI(t)
	var writes int
	steps := []tfresource.TestStep{{
		Config: vserverConfig(api, nil),
		Check:  checkMock(func() error { writes = api.WriteCount(); return nil }),
	}}
	for field, value := range map[string]string{
		"memory": "1024", "cores": "1", "disk": "20", "backups": "0", "traffic": `"1000"`,
		"duration_days": "90", "template_id": "12",
	} {
		steps = append(steps, tfresource.TestStep{
			Config:      vserverConfig(api, map[string]string{field: value}),
			ExpectError: re(`(?s)Purchase arguments cannot be changed.*` + field),
		})
	}
	steps = append(steps, tfresource.TestStep{
		// Switching to a packet is a change of the purchase too.
		Config:      packetConfig(api, nil),
		ExpectError: re(`(?s)Purchase arguments cannot be changed.*cores`),
	}, tfresource.TestStep{
		PreConfig: func() {
			if api.WriteCount() != writes || api.ChargeCount() != 1 {
				t.Errorf("writes %d → %d, charges %d: the plan errors must not write", writes, api.WriteCount(), api.ChargeCount())
			}
		},
		Config:   vserverConfig(api, nil),
		PlanOnly: true,
	})
	accTest(t, steps...)
}

func TestAccVserver_PacketChangeIsAPlanError(t *testing.T) {
	api := vserverAPI(t)
	accTest(t,
		tfresource.TestStep{Config: packetConfig(api, nil)},
		tfresource.TestStep{
			Config:      packetConfig(api, map[string]string{"packet_id": "1"}),
			ExpectError: re(`(?s)Purchase arguments cannot be changed.*packet_id`),
		},
		tfresource.TestStep{
			Config:      packetConfig(api, map[string]string{"duration_months": "12"}),
			ExpectError: re(`(?s)Purchase arguments cannot be changed.*duration_months`),
		},
		tfresource.TestStep{Config: packetConfig(api, nil), PlanOnly: true, Check: wantCharges(api, 1)},
	)
}

// Non-purchase arguments change in place without a purchase.
func TestAccVserver_DisplayNameAndAutoRenewUpdate(t *testing.T) {
	api := vserverAPI(t)
	var id string
	var orders int
	accTest(t,
		tfresource.TestStep{
			Config: vserverConfig(api, nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "display_name", "web1"),
				tfresource.TestCheckResourceAttrWith(vserverAddr, "id", func(v string) error {
					id = v
					orders = api.CallCount(http.MethodPost, "/orders")
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: vserverConfig(api, map[string]string{"display_name": `"Webserver Frankfurt"`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "display_name", "Webserver Frankfurt"),
				tfresource.TestCheckResourceAttrPtr(vserverAddr, "id", &id),
				wantCharges(api, 1),
				checkMock(func() error {
					if got := serverAttr(api, serverID(t, api), "display_name"); got != "Webserver Frankfurt" {
						return fmt.Errorf("display_name = %v", got)
					}
					if n := api.CallCount(http.MethodPost, "/orders"); n != orders {
						return fmt.Errorf("POST /orders %d → %d: a rename must not order", orders, n)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: vserverConfig(api, map[string]string{"display_name": `"Webserver Frankfurt"`, "auto_renew": "false"}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "auto_renew", "false"),
				checkMock(func() error {
					if got := serverAttr(api, serverID(t, api), "auto_renew"); got != false {
						return fmt.Errorf("auto_renew = %v", got)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config: vserverConfig(api, map[string]string{"display_name": `"Webserver Frankfurt"`, "auto_renew": "true",
				"promo_code": `"LATER"`, "max_price": "50", "ssh_key_ids": "[7]", "password": `"other-pass"`, "create_timeout": `"45m"`}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "auto_renew", "true"),
				wantCharges(api, 1),
				checkMock(func() error {
					if got := serverAttr(api, serverID(t, api), "auto_renew"); got != true {
						return fmt.Errorf("auto_renew = %v", got)
					}
					return nil
				}),
			),
		},
		// The panel takes 4–32 characters.
		tfresource.TestStep{
			Config:      vserverConfig(api, map[string]string{"display_name": `"web"`}),
			ExpectError: re(`(?s)display_name.*4 and 32`),
		},
		tfresource.TestStep{
			Config: vserverConfig(api, map[string]string{"display_name": `"Webserver Frankfurt"`, "auto_renew": "true",
				"promo_code": `"LATER"`, "max_price": "50", "ssh_key_ids": "[7]", "password": `"other-pass"`, "create_timeout": `"45m"`}),
			PlanOnly: true,
		},
	)
	if n := api.CallCount(http.MethodPut, "/rootserver/{id}/display-name"); n != 1 {
		t.Errorf("display-name PUTs = %d, want 1", n)
	}
}

// hostname is the client reference: renaming it is a plan error, no writes.
func TestAccVserver_HostnameChangeIsAPlanError(t *testing.T) {
	api := vserverAPI(t)
	var writes int
	accTest(t,
		tfresource.TestStep{
			Config: vserverConfig(api, nil),
			Check:  checkMock(func() error { writes = api.WriteCount(); return nil }),
		},
		tfresource.TestStep{
			Config:      vserverConfig(api, map[string]string{"hostname": `"web-frankfurt"`}),
			ExpectError: re(`(?s)Purchase arguments cannot be changed.*hostname.*identifies the purchased server.*display_name`),
		},
		tfresource.TestStep{
			PreConfig: func() {
				if api.WriteCount() != writes {
					t.Errorf("writes %d → %d", writes, api.WriteCount())
				}
			},
			Config:   vserverConfig(api, nil),
			PlanOnly: true,
		},
	)
}

// Destroy cancels: auto-renew off, the server stays until expires_at.
func TestAccVserver_DestroyCancels(t *testing.T) {
	api := vserverAPI(t)
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    []tfresource.TestStep{{Config: vserverConfig(api, nil)}},
		CheckDestroy: func(*terraform.State) error {
			id := serverID(t, api)
			if got := serverAttr(api, id, "auto_renew"); got != false {
				return fmt.Errorf("auto_renew = %v after destroy, want false", got)
			}
			if api.CallCount(http.MethodDelete, "/rootserver/{id}") != 0 {
				return fmt.Errorf("destroy must not delete")
			}
			return nil
		},
	})
}

// Review Focus 2: destroy, then apply the same configuration before the term
// ends → the order is replayed, the server taken over and auto-renew back on;
// nothing is bought twice.
func TestAccVserver_DestroyThenApplyAdopts(t *testing.T) {
	api := vserverAPI(t)
	var id string
	accTest(t,
		tfresource.TestStep{
			Config: vserverConfig(api, nil),
			Check: tfresource.TestCheckResourceAttrWith(vserverAddr, "id", func(v string) error {
				id = v
				return nil
			}),
		},
		tfresource.TestStep{Config: vserverConfig(api, nil), Destroy: true},
		tfresource.TestStep{
			PreConfig: func() {
				sid := serverID(t, api)
				if serverAttr(api, sid, "auto_renew") != false {
					t.Errorf("destroy did not switch auto-renew off")
				}
				backdateOrders(api, time.Hour) // the next apply comes later
			},
			Config: vserverConfig(api, nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrPtr(vserverAddr, "id", &id),
				tfresource.TestCheckResourceAttr(vserverAddr, "auto_renew", "true"),
				wantCharges(api, 1),
				checkMock(func() error {
					if got := serverAttr(api, serverID(t, api), "auto_renew"); got != true {
						return fmt.Errorf("auto_renew = %v, want true again", got)
					}
					return nil
				}),
			),
		},
	)
}

// Review Focus 1: the apply is aborted after the charge (timeout while the
// server installs); the next apply of the same configuration does not buy
// again.
func TestAccVserver_AbortedApplyIsNotBoughtTwice(t *testing.T) {
	api := vserverAPI(t)
	accTest(t,
		tfresource.TestStep{
			PreConfig:   func() { api.SetProvisioningPolls(1_000_000) },
			Config:      vserverConfig(api, map[string]string{"create_timeout": `"1s"`}),
			ExpectError: re(`(?s)already paid.*will not be bought twice`),
		},
		tfresource.TestStep{
			PreConfig: func() {
				if api.ChargeCount() != 1 {
					t.Fatalf("charges = %d after the aborted apply", api.ChargeCount())
				}
				for _, o := range api.OrderIDs() {
					api.SetOrderItemStatus(o, 0, client.StatusActive)
				}
				backdateOrders(api, 10*time.Minute)
			},
			Config: vserverConfig(api, nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "status", "active"),
				tfresource.TestCheckResourceAttrWith(vserverAddr, "id", func(v string) error {
					if v != strconv.FormatInt(serverID(t, api), 10) {
						return fmt.Errorf("id = %s, want the paid server", v)
					}
					return nil
				}),
				wantCharges(api, 1),
			),
		},
	)
}

// A live server with the hostname exists (CLIENT_REFERENCE_IN_USE), still
// installing: it is taken over, auto-renew switched on, and Create waits for
// the installation.
func TestAccVserver_AdoptsInstallingServer(t *testing.T) {
	api := vserverAPI(t)
	existing := api.SeedRootserver(map[string]any{"client_reference": "web1", "auto_renew": false, "installed": false, "display_name": "Panel Name"})
	api.SetInstallAfterReads(existing, 3)
	accTest(t, tfresource.TestStep{
		Config: vserverConfig(api, nil),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(vserverAddr, "id", strconv.FormatInt(existing, 10)),
			tfresource.TestCheckResourceAttr(vserverAddr, "status", "active"),
			tfresource.TestCheckNoResourceAttr(vserverAddr, "display_name"),
			checkMock(func() error {
				if got := serverAttr(api, existing, "display_name"); got != "Panel Name" {
					return fmt.Errorf("display_name = %v: a taken-over server keeps its panel name", got)
				}
				return nil
			}),
			wantCharges(api, 0),
			checkMock(func() error {
				if got := serverAttr(api, existing, "auto_renew"); got != true {
					return fmt.Errorf("auto_renew = %v", got)
				}
				return nil
			}),
		),
	}, tfresource.TestStep{Config: vserverConfig(api, nil), PlanOnly: true})
}

func TestAccVserver_AdoptedServerNotInstalledInTime(t *testing.T) {
	api := vserverAPI(t)
	existing := api.SeedRootserver(map[string]any{"client_reference": "web1", "installed": false})
	api.SetInstallAfterReads(existing, 1_000_000)
	accTest(t,
		tfresource.TestStep{
			Config:      vserverConfig(api, map[string]string{"create_timeout": `"1s"`}),
			ExpectError: re(`(?s)vServer ` + strconv.FormatInt(existing, 10) + ` was taken over, but it is not installed.*will not be bought twice`),
		},
		tfresource.TestStep{
			PreConfig: func() { api.SetInstallAfterReads(existing, 0) },
			Config:    vserverConfig(api, nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "id", strconv.FormatInt(existing, 10)),
				wantCharges(api, 0),
			),
		},
	)
}

// The hostname belongs to a live server with another configuration: refused,
// nothing taken over or changed.
func TestAccVserver_AdoptionOfDifferentServerIsRefused(t *testing.T) {
	api := vserverAPI(t)
	existing := api.SeedRootserver(map[string]any{"client_reference": "web1", "memory": 1024, "auto_renew": false})
	accTest(t, tfresource.TestStep{
		Config:      vserverConfig(api, nil),
		ExpectError: re(`(?s)hostname.*web1.*` + strconv.FormatInt(existing, 10) + `.*memory`),
	})
	if got := serverAttr(api, existing, "auto_renew"); got != false {
		t.Errorf("auto_renew = %v: a refused server must not be touched", got)
	}
	if api.ChargeCount() != 0 {
		t.Errorf("charges = %d", api.ChargeCount())
	}
}

// Review Focus 5: deleted outside Terraform → removed from the state, the
// plan proposes a new server, no crash.
func TestAccVserver_DeletedIsRemovedFromState(t *testing.T) {
	api := vserverAPI(t)
	accTest(t,
		tfresource.TestStep{Config: vserverConfig(api, nil)},
		tfresource.TestStep{
			PreConfig:          func() { api.DeleteResource(testapi.ResourceRootserver, serverID(t, api)) },
			RefreshState:       true,
			ExpectNonEmptyPlan: true,
			Check: func(s *terraform.State) error {
				if rs := s.RootModule().Resources[vserverAddr]; rs != nil {
					return fmt.Errorf("still in state: %v", rs.Primary.ID)
				}
				return nil
			},
		},
		tfresource.TestStep{Config: vserverConfig(api, nil), PlanOnly: true, ExpectNonEmptyPlan: true},
	)
}

// Review Focus 5 (fix round 1): expired but still listed by the API → stays
// in the state as "expired" (the platform may still renew it), and the next
// apply buys nothing.
func TestAccVserver_ExpiredStaysInStateWithoutPurchase(t *testing.T) {
	api := vserverAPI(t)
	accTest(t,
		tfresource.TestStep{Config: vserverConfig(api, nil)},
		tfresource.TestStep{
			PreConfig: func() {
				api.UpdateResource(testapi.ResourceRootserver, serverID(t, api), func(a map[string]any) {
					a["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
				})
			},
			Config: vserverConfig(api, nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "status", "expired"),
				wantCharges(api, 1),
				checkMock(func() error {
					if n := api.CallCount(http.MethodPost, "/orders"); n != 1 {
						return fmt.Errorf("POST /orders = %d, want 1", n)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{Config: vserverConfig(api, nil), PlanOnly: true},
	)
}

func TestAccVserver_InsufficientBalance(t *testing.T) {
	api := vserverAPI(t)
	api.SetBalance(1.5)
	accTest(t, tfresource.TestStep{
		Config:      vserverConfig(api, nil),
		ExpectError: re(`(?s)8\.49 EUR short.*Top up the balance`),
	})
	if api.ChargeCount() != 0 || len(api.ResourceIDs(testapi.ResourceRootserver)) != 0 {
		t.Errorf("charges = %d, servers = %v", api.ChargeCount(), api.ResourceIDs(testapi.ResourceRootserver))
	}
}

func TestAccVserver_MaxPrice(t *testing.T) {
	api := vserverAPI(t)
	accTest(t,
		tfresource.TestStep{
			Config:      vserverConfig(api, map[string]string{"max_price": "9.98"}),
			ExpectError: re(`(?s)Price above max_price.*9\.99`),
		},
		tfresource.TestStep{
			PreConfig: func() {
				if api.CallCount(http.MethodPost, "/orders") != 0 {
					t.Errorf("max_price exceeded must not order")
				}
			},
			Config: vserverConfig(api, map[string]string{"max_price": "9.99"}),
			Check:  wantCharges(api, 1),
		},
	)
}

func TestAccVserver_Import(t *testing.T) {
	api := vserverAPI(t)
	accTest(t,
		tfresource.TestStep{Config: vserverConfig(api, nil)},
		tfresource.TestStep{
			ResourceName:      vserverAddr,
			ImportState:       true,
			ImportStateVerify: true,
			// Not readable from the API (only used for the purchase, or write-only).
			ImportStateVerifyIgnore: []string{"hostname", "display_name", "traffic", "duration_days", "ssh_key_ids", "password",
				"promo_code", "max_price", "create_timeout"},
		},
	)
}

// A server bought outside Terraform is imported; the first apply takes the
// configured hostname as is (no purchase, no rename).
func TestAccVserver_ImportExisting(t *testing.T) {
	api := vserverAPI(t)
	existing := api.SeedRootserver(map[string]any{"display_name": "old-name"})
	id := strconv.FormatInt(existing, 10)
	accTest(t,
		initStep(api),
		tfresource.TestStep{
			Config:             importConfig(vserverConfig(api, nil)),
			ResourceName:       vserverAddr,
			ImportState:        true,
			ImportStateId:      id,
			ImportStatePersist: true,
			ImportStateCheck: func(states []*terraform.InstanceState) error {
				if len(states) != 1 || states[0].Attributes["memory"] != "2048" || states[0].Attributes["auto_renew"] != "true" ||
					states[0].Attributes["template_id"] != "11" {
					return fmt.Errorf("imported = %v", states)
				}
				return nil
			},
		},
		tfresource.TestStep{
			Config: vserverConfig(api, nil),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(vserverAddr, "id", id),
				wantCharges(api, 0),
				checkMock(func() error {
					if got := serverAttr(api, existing, "display_name"); got != "old-name" {
						return fmt.Errorf("display_name = %v", got)
					}
					return nil
				}),
			),
		},
		tfresource.TestStep{
			Config:        importConfig(vserverConfig(api, nil)),
			ResourceName:  vserverAddr,
			ImportState:   true,
			ImportStateId: "abc",
			ExpectError:   re("numeric"),
		},
	)
}

// ---- direct calls (warnings are not visible through terraform-plugin-testing) ----

func vserverSchema(t *testing.T) resource.SchemaResponse {
	t.Helper()
	var sch resource.SchemaResponse
	NewVserverResource().Schema(context.Background(), resource.SchemaRequest{}, &sch)
	return sch
}

// tfValue builds an object of the vServer schema: set values, computed ones
// unknown (or null when nullComputed), the rest null.
func tfValue(t *testing.T, sch resource.SchemaResponse, set map[string]tftypes.Value, nullComputed bool) tftypes.Value {
	t.Helper()
	ctx := context.Background()
	values := map[string]tftypes.Value{}
	for name, attr := range sch.Schema.Attributes {
		typ := attr.GetType().TerraformType(ctx)
		switch v, ok := set[name]; {
		case ok:
			values[name] = v
		case attr.IsComputed() && !nullComputed:
			values[name] = tftypes.NewValue(typ, tftypes.UnknownValue)
		default:
			values[name] = tftypes.NewValue(typ, nil)
		}
	}
	return tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), values)
}

func str(s string) tftypes.Value  { return tftypes.NewValue(tftypes.String, s) }
func num(n float64) tftypes.Value { return tftypes.NewValue(tftypes.Number, n) }

func customValues() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"hostname": str("web1"), "cores": num(2), "memory": num(2048), "disk": num(40), "addresses": num(1),
		"backups": num(1), "traffic": str("unlimited"), "duration_days": num(30), "template_id": num(11),
		"auto_renew": tftypes.NewValue(tftypes.Bool, true), "create_timeout": str("20s"),
	}
}

func testVserverResource(api *testapi.Server) *vserverResource {
	res := NewVserverResource().(*vserverResource) //nolint:forcetypeassert // test
	res.client = client.New(api.URL, api.Token, client.WithRetryBackoff(time.Millisecond), client.WithPollInterval(10*time.Millisecond))
	return res
}

func TestVserver_DestroyWarnsWithExpiry(t *testing.T) {
	api := vserverAPI(t)
	id := api.SeedRootserver(map[string]any{"expires_at": "2030-01-02T03:04:05Z"})
	sch := vserverSchema(t)
	vals := customValues()
	vals["id"] = str(strconv.FormatInt(id, 10))
	vals["expires_at"] = str("2030-01-02T03:04:05Z")
	req := resource.DeleteRequest{State: tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}}
	var resp resource.DeleteResponse

	testVserverResource(api).Delete(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics)
	}
	w := resp.Diagnostics.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].Detail(), "2030-01-02") || !strings.Contains(w[0].Detail(), "no refund") ||
		!strings.Contains(w[0].Summary(), "cancelled") {
		t.Errorf("want one cancel warning with the expiry date and 'no refund', got %v", resp.Diagnostics)
	}
	if serverAttr(api, id, "auto_renew") != false {
		t.Errorf("auto-renew not switched off")
	}
}

func TestVserver_DestroyOfGoneServerSucceeds(t *testing.T) {
	api := vserverAPI(t)
	sch := vserverSchema(t)
	vals := customValues()
	vals["id"] = str("4711")
	req := resource.DeleteRequest{State: tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}}
	var resp resource.DeleteResponse
	testVserverResource(api).Delete(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a gone server must succeed: %v", resp.Diagnostics)
	}
}

func createDirect(t *testing.T, api *testapi.Server) resource.CreateResponse {
	t.Helper()
	sch := vserverSchema(t)
	plan := tfValue(t, sch, customValues(), false)
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch.Schema, Raw: plan},
		Config: tfsdk.Config{Schema: sch.Schema, Raw: tfValue(t, sch, customValues(), true)},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}}
	testVserverResource(api).Create(context.Background(), req, &resp)
	return resp
}

func TestVserver_AdoptionWarns(t *testing.T) {
	api := vserverAPI(t)
	existing := api.SeedRootserver(map[string]any{"client_reference": "web1", "auto_renew": false})

	resp := createDirect(t, api)

	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	w := resp.Diagnostics.Warnings()
	if len(w) != 1 || w[0].Summary() != "Existing vServer taken over" || !strings.Contains(w[0].Detail(), strconv.FormatInt(existing, 10)) ||
		!strings.Contains(w[0].Detail(), "Auto-renew is on") {
		t.Errorf("want the take-over warning naming %d, got %v", existing, resp.Diagnostics)
	}
	// Follow-up T6: the panel name of a taken-over server is unknown (not
	// readable); it is not claimed to be the hostname.
	var dn types.String
	resp.State.GetAttribute(context.Background(), path.Root("display_name"), &dn)
	if !dn.IsNull() {
		t.Errorf("display_name = %v, want null for a taken-over server without configured display_name", dn)
	}
}

func TestVserver_NewOrderDoesNotWarn(t *testing.T) {
	api := vserverAPI(t)
	resp := createDirect(t, api)
	if resp.Diagnostics.HasError() || len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("a plain purchase has no diagnostics, got %v", resp.Diagnostics)
	}
	if n := api.CallCount(http.MethodPut, "/rootserver/{id}/autorenew"); n != 0 {
		t.Errorf("autorenew PUTs = %d: a new server already renews", n)
	}
}

func readDirect(t *testing.T, api *testapi.Server, id int64, set map[string]tftypes.Value) resource.ReadResponse {
	t.Helper()
	sch := vserverSchema(t)
	vals := customValues()
	vals["id"] = str(strconv.FormatInt(id, 10))
	for k, v := range set {
		vals[k] = v
	}
	state := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, vals, true)}
	resp := resource.ReadResponse{State: state}
	testVserverResource(api).Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	return resp
}

func stateString(t *testing.T, resp resource.ReadResponse, name string) string {
	t.Helper()
	var v types.String
	resp.State.GetAttribute(context.Background(), path.Root(name), &v)
	return v.ValueString()
}

func TestVserver_ReadExpiredWarnsAndKeeps(t *testing.T) {
	api := vserverAPI(t)
	id := api.SeedRootserver(map[string]any{"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	resp := readDirect(t, api, id, nil)
	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("want the server kept, got %v (state null: %v)", resp.Diagnostics, resp.State.Raw.IsNull())
	}
	if got := stateString(t, resp, "status"); got != "expired" {
		t.Errorf("status = %q", got)
	}
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "expired on") ||
		!strings.Contains(w[0].Detail(), "destroy it to stop renewal") {
		t.Errorf("want an expiry warning, got %v", resp.Diagnostics)
	}
}

// Follow-up T6: with auto-renew off the warning does not speak of a late renewal.
func TestVserver_ReadExpiredWithAutoRenewOff(t *testing.T) {
	api := vserverAPI(t)
	id := api.SeedRootserver(map[string]any{"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "auto_renew": false})
	resp := readDirect(t, api, id, nil)
	w := resp.Diagnostics.Warnings()
	if resp.Diagnostics.HasError() || len(w) != 1 || !strings.Contains(w[0].Detail(), "expired on") ||
		!strings.Contains(w[0].Detail(), "Auto-renew is off") || strings.Contains(w[0].Detail(), "may still renew") {
		t.Errorf("want an expiry warning without the late-renewal hint, got %v", resp.Diagnostics)
	}
}

// A route the API does not have (wrong endpoint, older API) answers 404
// ENDPOINT_NOT_FOUND. That says nothing about the server: Read fails and the
// state is kept, while a real NOT_FOUND removes it.
func TestVserver_ReadEndpointNotFoundKeepsState(t *testing.T) {
	api := vserverAPI(t)
	id := api.SeedRootserver(nil)
	api.InjectResponse(http.MethodGet, "/rootserver/{id}", 1,
		testapi.Fail(http.StatusNotFound, client.CodeEndpointNotFound, "Endpoint nicht gefunden.", nil))
	resp := readDirect(t, api, id, nil)
	if !resp.Diagnostics.HasError() || !strings.Contains(errorText(resp.Diagnostics), client.CodeEndpointNotFound) {
		t.Fatalf("want an error naming ENDPOINT_NOT_FOUND, got %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Errorf("a missing route must not remove the server from the state")
	}
}

// Minor 3: renew is owner-only; a 404 there while show and rental answer
// means shared access, not "gone".
func TestVserver_ReadRenewNotFoundIsNotTheOwner(t *testing.T) {
	api := vserverAPI(t)
	id := api.SeedRootserver(nil)
	api.InjectResponse(http.MethodGet, "/rootserver/{id}/renew", 1,
		testapi.Fail(http.StatusNotFound, "NOT_FOUND", "Endpoint nicht gefunden.", nil))
	resp := readDirect(t, api, id, nil)
	if !resp.Diagnostics.HasError() || !strings.Contains(errorText(resp.Diagnostics), "not its owner") {
		t.Fatalf("want the owner error, got %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Errorf("a shared server must not be removed from the state")
	}
}

// Minor 4: the renewal info fails on its own → warning, last value kept.
func TestVserver_ReadRenewFailureKeepsAutoRenew(t *testing.T) {
	api := vserverAPI(t)
	id := api.SeedRootserver(map[string]any{"auto_renew": true})
	api.InjectResponse(http.MethodGet, "/rootserver/{id}/renew", 1,
		testapi.Fail(http.StatusUnprocessableEntity, "PRICE_UNAVAILABLE", "Preis nicht verfügbar.", nil))
	resp := readDirect(t, api, id, map[string]tftypes.Value{"auto_renew": tftypes.NewValue(tftypes.Bool, false)})
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var auto types.Bool
	resp.State.GetAttribute(context.Background(), path.Root("auto_renew"), &auto)
	if auto.ValueBool() {
		t.Errorf("auto_renew = true, want the last known false")
	}
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "PRICE_UNAVAILABLE") {
		t.Errorf("want the warning, got %v", resp.Diagnostics)
	}
}

func TestVserver_DestroyOfExpiredServerSaysSo(t *testing.T) {
	text := cancelText("1001", types.StringValue("2020-01-02T03:04:05Z"), "Cancelling")
	if !strings.Contains(text, "already expired") || strings.Contains(text, "takes it over") {
		t.Errorf("text = %q", text)
	}
	text = cancelText("1001", types.StringValue("2099-01-02T03:04:05Z"), "Cancelling")
	if !strings.Contains(text, "same hostname") || !strings.Contains(text, "2099-01-02") {
		t.Errorf("text = %q", text)
	}
}

// The order body is stable: ssh_key_ids sorted, so the API's request
// fingerprint and the Idempotency-Key match between runs.
func TestVserver_OrderItemIsCanonical(t *testing.T) {
	m := vserverModel{SSHKeyIDs: mustSet(t, 9, 2, 5)}
	ids, diags := m.sshKeyIDs(context.Background())
	if diags.HasError() {
		t.Fatal(diags)
	}
	raw, _ := json.Marshal(ids)
	if string(raw) != "[2,5,9]" {
		t.Errorf("ssh_key_ids = %s", raw)
	}
}

func mustSet(t *testing.T, ids ...int64) types.Set {
	t.Helper()
	s, diags := types.SetValueFrom(context.Background(), types.Int64Type, ids)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return s
}

func modifyPlanDirect(t *testing.T, state, plan map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	sch := vserverSchema(t)
	st := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, state, true)}
	pl := tfsdk.Plan{Schema: sch.Schema, Raw: tfValue(t, sch, plan, true)}
	resp := resource.ModifyPlanResponse{Plan: pl}
	NewVserverResource().(*vserverResource).ModifyPlan(context.Background(), //nolint:forcetypeassert // test
		resource.ModifyPlanRequest{State: st, Plan: pl, Config: tfsdk.Config{Schema: sch.Schema, Raw: pl.Raw}}, &resp)
	return resp
}

func TestVserver_HostnameChangeIsRefusedAtPlan(t *testing.T) {
	state := customValues()
	state["id"] = str("1001")
	plan := customValues()
	plan["id"] = str("1001")
	plan["hostname"] = str("web-frankfurt")

	resp := modifyPlanDirect(t, state, plan)
	if !strings.Contains(errorText(resp.Diagnostics), "identifies the purchased server") {
		t.Errorf("want the hostname plan error, got %v", resp.Diagnostics)
	}
}

// After an import the hostname is unknown: the configured one is taken.
func TestVserver_HostnameAfterImportIsTaken(t *testing.T) {
	state := customValues()
	state["id"] = str("1001")
	delete(state, "hostname")
	plan := customValues()
	plan["id"] = str("1001")
	if resp := modifyPlanDirect(t, state, plan); resp.Diagnostics.HasError() || len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("want no diagnostics, got %v", resp.Diagnostics)
	}
}

func TestVserver_DestroyPlanWarns(t *testing.T) {
	state := customValues()
	state["id"] = str("1001")
	state["expires_at"] = str("2030-01-02T03:04:05Z")
	sch := vserverSchema(t)
	st := tfsdk.State{Schema: sch.Schema, Raw: tfValue(t, sch, state, true)}
	pl := tfsdk.Plan{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(context.Background()), nil)}
	resp := resource.ModifyPlanResponse{Plan: pl}
	NewVserverResource().(*vserverResource).ModifyPlan(context.Background(), resource.ModifyPlanRequest{State: st, Plan: pl}, &resp) //nolint:forcetypeassert // test
	if w := resp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "2030-01-02") || !strings.Contains(w[0].Detail(), "no refund") {
		t.Errorf("want the destroy warning, got %v", resp.Diagnostics)
	}
}
