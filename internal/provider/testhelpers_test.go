// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

// providerConfig is the provider block pointing at the mock API.
func providerConfig(api *testapi.Server) string {
	return fmt.Sprintf(`
provider "prepaidhost" {
  token    = %q
  endpoint = %q
}
`, api.Token, api.URL)
}

// accTest runs a resource.Test with the in-process provider.
func accTest(t *testing.T, steps ...resource.TestStep) {
	t.Helper()
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
	})
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// importConfig adds the required_providers block to a config used by an import
// step: terraform-plugin-testing adds it to normal steps only, and without it
// the CLI looks for hashicorp/prepaidhost.
func importConfig(cfg string) string {
	host := os.Getenv("TF_ACC_PROVIDER_HOST")
	if host == "" {
		host = "registry.terraform.io"
	}
	return fmt.Sprintf(`
terraform {
  required_providers {
    prepaidhost = {
      source = "%s/prepaid-host/prepaidhost"
    }
  }
}
`, host) + cfg
}

// initStep is a first step that only starts the provider, so that an import
// step can persist its state into the working directory of the test.
func initStep(api *testapi.Server) resource.TestStep {
	return resource.TestStep{Config: providerConfig(api) + `data "prepaidhost_balance" "init" {}`}
}
