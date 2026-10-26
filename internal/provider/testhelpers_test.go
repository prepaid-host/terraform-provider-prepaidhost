// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
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
