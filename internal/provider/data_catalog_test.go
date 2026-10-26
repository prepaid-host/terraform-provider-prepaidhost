// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

func TestAccCatalogDataSource_Rootserver(t *testing.T) {
	api := testapi.New(t)
	const addr = "data.prepaidhost_catalog.this"

	accTest(t, resource.TestStep{
		Config: providerConfig(api) + `data "prepaidhost_catalog" "this" { type = "rootserver" }`,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(addr, "type", "rootserver"),
			resource.TestCheckResourceAttr(addr, "options.cores.#", "2"),
			resource.TestCheckResourceAttr(addr, "options.cores.1.value", "2"),
			resource.TestCheckResourceAttr(addr, "options.cores.1.name", "2 vCores"),
			resource.TestCheckResourceAttr(addr, "options.cores.1.price_monthly", "3"),
			resource.TestCheckResourceAttr(addr, "options.traffic.0.value", "unlimited"),
			resource.TestCheckResourceAttr(addr, "durations.#", "4"),
			resource.TestCheckResourceAttr(addr, "durations.3.days", "365"),
			resource.TestCheckResourceAttr(addr, "durations.3.months", "12"),
			resource.TestCheckResourceAttr(addr, "durations.3.discount_percent", "10"),
			resource.TestCheckResourceAttr(addr, "templates.#", "2"),
			resource.TestCheckResourceAttr(addr, "templates.0.id", "11"),
			resource.TestCheckResourceAttr(addr, "templates.0.name", "Debian 12"),
			resource.TestCheckResourceAttr(addr, "templates.0.os_family", "linux"),
			// Attributes of the other types stay empty.
			resource.TestCheckNoResourceAttr(addr, "packets.#"),
			resource.TestCheckNoResourceAttr(addr, "tlds.#"),
		),
	})
}

func TestAccCatalogDataSource_Rootpacket(t *testing.T) {
	api := testapi.New(t)
	const addr = "data.prepaidhost_catalog.this"

	accTest(t, resource.TestStep{
		Config: providerConfig(api) + `data "prepaidhost_catalog" "this" { type = "rootpacket" }`,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(addr, "packets.#", "2"),
			resource.TestCheckResourceAttr(addr, "packets.1.id", "2"),
			resource.TestCheckResourceAttr(addr, "packets.1.name", "Plus"),
			resource.TestCheckResourceAttr(addr, "packets.1.cores", "2"),
			resource.TestCheckResourceAttr(addr, "packets.1.memory", "2048"),
			resource.TestCheckResourceAttr(addr, "packets.1.disk", "40"),
			resource.TestCheckResourceAttr(addr, "packets.1.addresses", "1"),
			resource.TestCheckResourceAttr(addr, "packets.1.backups", "2"),
			resource.TestCheckResourceAttr(addr, "packets.1.traffic", "unlimited"),
			resource.TestCheckResourceAttr(addr, "packets.1.price_monthly", "9.99"),
			resource.TestCheckResourceAttr(addr, "packets.1.active_promo_percent", "15"),
			resource.TestCheckResourceAttr(addr, "durations_months.#", "4"),
			resource.TestCheckResourceAttr(addr, "durations_months.3", "12"),
			resource.TestCheckResourceAttr(addr, "templates.#", "2"),
			resource.TestCheckNoResourceAttr(addr, "options.%"),
		),
	})
}

func TestAccCatalogDataSource_Domain(t *testing.T) {
	api := testapi.New(t)
	const addr = "data.prepaidhost_catalog.this"

	accTest(t, resource.TestStep{
		Config: providerConfig(api) + `data "prepaidhost_catalog" "this" { type = "domain" }`,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(addr, "tlds.#", "2"),
			resource.TestCheckResourceAttr(addr, "tlds.0.tld", "com"),
			resource.TestCheckResourceAttr(addr, "tlds.0.price_yearly", "11.99"),
			// No discount: null, not 0.
			resource.TestCheckNoResourceAttr(addr, "tlds.0.price_yearly_discounted"),
			resource.TestCheckResourceAttr(addr, "tlds.1.price_yearly_discounted", "4.99"),
			resource.TestCheckResourceAttr(addr, "durations_months.#", "3"),
			resource.TestCheckResourceAttr(addr, "durations_months.2", "36"),
			resource.TestCheckNoResourceAttr(addr, "templates.#"),
		),
	})
}

func TestAccCatalogDataSource_InvalidType(t *testing.T) {
	api := testapi.New(t)
	accTest(t, resource.TestStep{
		Config:      providerConfig(api) + `data "prepaidhost_catalog" "this" { type = "webhosting" }`,
		ExpectError: re(`(?s)type.*(rootserver|one of)`),
	})
	if api.CallCount("GET", "/catalog/{type}") != 0 {
		t.Error("invalid type must be rejected before any API call")
	}
}

func TestAccCatalogDataSource_APIError(t *testing.T) {
	api := testapi.New(t)
	api.InjectResponse("GET", "/catalog/{type}", 10,
		testapi.Fail(401, "UNAUTHENTICATED", "Unauthenticated.", nil))

	accTest(t, resource.TestStep{
		Config:      providerConfig(api) + `data "prepaidhost_catalog" "this" { type = "domain" }`,
		ExpectError: re(`UNAUTHENTICATED`),
	})
}

func testStepCatalog(api *testapi.Server, addr string) resource.TestStep {
	return resource.TestStep{
		Config: providerConfig(api) + `data "prepaidhost_catalog" "this" { type = "rootpacket" }`,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(addr, "packets.0.cores", "2"),
			resource.TestCheckResourceAttr(addr, "packets.0.memory", "2048"),
			resource.TestCheckResourceAttr(addr, "packets.0.disk", "40"),
			resource.TestCheckResourceAttr(addr, "packets.0.addresses", "1"),
			resource.TestCheckResourceAttr(addr, "packets.0.backups", "3"),
			resource.TestCheckResourceAttr(addr, "packets.0.traffic", "1000"),
		),
	}
}

func TestAccCatalogDataSource_PacketNumbersAsStrings(t *testing.T) {
	api := testapi.New(t)
	api.SetCatalog("rootpacket", map[string]any{
		"packets": []any{map[string]any{
			"id": "7", "name": "Str", "cores": "2", "memory": "2048", "disk": "40", "addresses": "1", "backups": "3",
			"traffic": 1000, "price_monthly": 5.0, "active_promo_percent": 0.0,
		}},
		"durations_months": []any{1},
		"templates":        []any{},
	})
	const addr = "data.prepaidhost_catalog.this"
	accTest(t, testStepCatalog(api, addr))
}

func TestFlexInt_NullAndEmptyAreZero(t *testing.T) {
	cases := map[string]int64{
		`null`:   0,
		`""`:     0,
		`"  "`:   0,
		`7`:      7,
		`"8"`:    8,
		`4.0`:    4,
		`" 12 "`: 12,
	}
	for in, want := range cases {
		var f flexInt
		if err := json.Unmarshal([]byte(in), &f); err != nil {
			t.Errorf("Unmarshal(%s): %v", in, err)
			continue
		}
		if int64(f) != want {
			t.Errorf("Unmarshal(%s) = %d, want %d", in, f, want)
		}
	}
	for _, in := range []string{`"abc"`, `true`, `{}`, `[]`} {
		var f flexInt
		if err := json.Unmarshal([]byte(in), &f); err == nil {
			t.Errorf("Unmarshal(%s): expected an error, got %d", in, f)
		}
	}
}

func TestFlexInt_NullInsidePacket(t *testing.T) {
	var p struct {
		ID      flexInt `json:"id"`
		Backups flexInt `json:"backups"`
	}
	if err := json.Unmarshal([]byte(`{"id":3,"backups":null}`), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.ID != 3 || p.Backups != 0 {
		t.Fatalf("got id=%d backups=%d", p.ID, p.Backups)
	}
}
