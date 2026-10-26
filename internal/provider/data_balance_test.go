// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

func TestAccBalanceDataSource(t *testing.T) {
	api := testapi.New(t)
	api.SetBalance(42.5)

	accTest(t, resource.TestStep{
		Config: providerConfig(api) + `data "prepaidhost_balance" "this" {}`,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.prepaidhost_balance.this", "amount", "42.5"),
			resource.TestCheckResourceAttr("data.prepaidhost_balance.this", "currency", "EUR"),
		),
	})
}

func TestAccBalanceDataSource_APIError(t *testing.T) {
	api := testapi.New(t)
	api.InjectResponse("GET", "/account/balance", 10,
		testapi.Fail(403, "INSUFFICIENT_SCOPE", "Token lacks scope account:billing.", nil))

	accTest(t, resource.TestStep{
		Config:      providerConfig(api) + `data "prepaidhost_balance" "this" {}`,
		ExpectError: re(`(?s)INSUFFICIENT_SCOPE.*account:billing`),
	})
}
