// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

// testAccProtoV6ProviderFactories starts the provider in-process for terraform-plugin-testing.
// Acceptance-style tests point the provider at internal/testapi, never at the real API.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"prepaidhost": providerserver.NewProtocol6WithError(newWithClientOptions("test",
		client.WithRetryBackoff(time.Millisecond),
		client.WithPollInterval(10*time.Millisecond),
	)()),
}

// testAccPreCheck is the hook for checks before an acceptance test runs.
// Nothing is required yet: every test brings its own mock API.
func testAccPreCheck(t *testing.T) {
	t.Helper()
}

// configure runs Configure on a fresh provider with the given attribute values
// (nil = attribute not set).
func configure(t *testing.T, token, endpoint *string) *provider.ConfigureResponse {
	t.Helper()
	ctx := context.Background()
	p := New("test")()

	var schemaResp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics)
	}

	str := func(v *string) tftypes.Value {
		if v == nil {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return tftypes.NewValue(tftypes.String, *v)
	}
	raw := tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"token":    str(token),
		"endpoint": str(endpoint),
	})

	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, provider.ConfigureRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw}}, resp)
	return resp
}

func ptr(s string) *string { return &s }

func TestProviderSchema(t *testing.T) {
	var resp provider.SchemaResponse
	New("test")().Schema(context.Background(), provider.SchemaRequest{}, &resp)

	token, ok := resp.Schema.Attributes["token"]
	if !ok || !token.IsSensitive() || !token.IsOptional() {
		t.Errorf("token attribute must be optional and sensitive: %+v", token)
	}
	endpoint, ok := resp.Schema.Attributes["endpoint"]
	if !ok || !endpoint.IsOptional() || endpoint.IsSensitive() {
		t.Errorf("endpoint attribute must be optional: %+v", endpoint)
	}
}

func TestProviderMetadata(t *testing.T) {
	var resp provider.MetadataResponse
	New("1.2.3")().Metadata(context.Background(), provider.MetadataRequest{}, &resp)
	if resp.TypeName != "prepaidhost" || resp.Version != "1.2.3" {
		t.Errorf("metadata = %+v", resp)
	}
}

func TestConfigure_ExplicitValues(t *testing.T) {
	t.Setenv(EnvToken, "")
	t.Setenv(EnvEndpoint, "")

	resp := configure(t, ptr("config-token"), ptr("http://127.0.0.1:9/api"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("diags: %v", resp.Diagnostics)
	}
	c, ok := resp.ResourceData.(*client.Client)
	if !ok {
		t.Fatalf("ResourceData = %T, want *client.Client", resp.ResourceData)
	}
	if c.Endpoint() != "http://127.0.0.1:9/api" || c.Token() != "config-token" {
		t.Errorf("client endpoint=%q token=%q", c.Endpoint(), c.Token())
	}
	if ds, ok := resp.DataSourceData.(*client.Client); !ok || ds != c {
		t.Errorf("DataSourceData must be the same client")
	}
}

func TestConfigure_EnvFallback(t *testing.T) {
	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvEndpoint, "https://staging.example.test/api")

	resp := configure(t, nil, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("diags: %v", resp.Diagnostics)
	}
	c := resp.ResourceData.(*client.Client) //nolint:forcetypeassert // test
	if c.Token() != "env-token" || c.Endpoint() != "https://staging.example.test/api" {
		t.Errorf("client endpoint=%q token=%q", c.Endpoint(), c.Token())
	}
}

func TestConfigure_ConfigWinsOverEnv(t *testing.T) {
	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvEndpoint, "https://env.example.test/api")

	resp := configure(t, ptr("config-token"), ptr("https://config.example.test/api"))
	c := resp.ResourceData.(*client.Client) //nolint:forcetypeassert // test
	if c.Token() != "config-token" || c.Endpoint() != "https://config.example.test/api" {
		t.Errorf("client endpoint=%q token=%q", c.Endpoint(), c.Token())
	}
}

func TestConfigure_DefaultEndpoint(t *testing.T) {
	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvEndpoint, "")

	resp := configure(t, nil, nil)
	c := resp.ResourceData.(*client.Client) //nolint:forcetypeassert // test
	if c.Endpoint() != DefaultEndpoint || DefaultEndpoint != "https://prepaid-host.com/api" {
		t.Errorf("endpoint = %q", c.Endpoint())
	}
}

func TestConfigure_MissingTokenIsDiagnostic(t *testing.T) {
	t.Setenv(EnvToken, "")
	t.Setenv(EnvEndpoint, "")

	resp := configure(t, nil, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("want error diagnostic")
	}
	d := resp.Diagnostics.Errors()[0]
	if !strings.Contains(d.Summary(), "token") || !strings.Contains(d.Detail(), EnvToken) {
		t.Errorf("diagnostic = %q / %q", d.Summary(), d.Detail())
	}
	if resp.ResourceData != nil {
		t.Errorf("no client on error")
	}
}

func TestConfigure_InvalidEndpointIsDiagnostic(t *testing.T) {
	t.Setenv(EnvToken, "")
	for _, ep := range []string{"ftp://example.test", "not a url", "https://"} {
		resp := configure(t, ptr("t"), ptr(ep))
		if !resp.Diagnostics.HasError() {
			t.Errorf("%q: want error diagnostic", ep)
		}
	}
}

// TestAccProvider_Loads checks that the CLI (OpenTofu or Terraform) can start
// and configure the provider. Runs only with TF_ACC=1.
func TestAccProvider_Loads(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "prepaidhost" {
  token    = "test-token"
  endpoint = "http://127.0.0.1:9/api"
}

output "ok" {
  value = "ok"
}
`,
			},
		},
	})
}
