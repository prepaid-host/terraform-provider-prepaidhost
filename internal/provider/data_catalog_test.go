// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
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
