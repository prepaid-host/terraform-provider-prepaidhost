// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

// Package provider implements the prepaidhost Terraform/OpenTofu provider.
package provider

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

const (
	// DefaultEndpoint is the production API.
	DefaultEndpoint = "https://prepaid-host.com/api"
	// EnvToken is the environment variable read when token is not configured.
	EnvToken = "PREPAIDHOST_TOKEN"
	// EnvEndpoint is the environment variable read when endpoint is not configured.
	EnvEndpoint = "PREPAIDHOST_ENDPOINT"
)

var _ provider.Provider = &PrepaidHostProvider{}

// PrepaidHostProvider is the provider implementation.
type PrepaidHostProvider struct {
	// version is set on release, "dev" for local builds and "test" in tests.
	version string
	// clientOptions are appended when the API client is built (tests use
	// them for short retry and poll intervals).
	clientOptions []client.Option
}

// PrepaidHostProviderModel is the provider configuration.
type PrepaidHostProviderModel struct {
	Token    types.String `tfsdk:"token"`
	Endpoint types.String `tfsdk:"endpoint"`
}

// New returns the provider factory.
func New(version string) func() provider.Provider {
	return newWithClientOptions(version)
}

func newWithClientOptions(version string, opts ...client.Option) func() provider.Provider {
	return func() provider.Provider {
		return &PrepaidHostProvider{version: version, clientOptions: opts}
	}
}

// Metadata sets the type name prefix of all resources and data sources.
func (p *PrepaidHostProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "prepaidhost"
	resp.Version = p.version
}

// Schema defines the provider configuration.
func (p *PrepaidHostProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage Prepaid-Host vServers, domains, DNS records, SSH keys and domain contacts. " +
			"Orders are paid from the prepaid balance of the account.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				MarkdownDescription: "Personal API Bearer token from the customer panel. Can also be set with the `" + EnvToken + "` environment variable. " +
					"Ordering needs a token created with the order scopes (`rootserver:order`, `domain:order`).",
				Optional:  true,
				Sensitive: true,
			},
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "API base URL. Defaults to `" + DefaultEndpoint + "`; can also be set with the `" + EnvEndpoint + "` environment variable.",
				Optional:            true,
			},
		},
	}
}

// Configure builds the API client shared by all resources and data sources.
func (p *PrepaidHostProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg PrepaidHostProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if cfg.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Unknown Prepaid-Host API token",
			"The token is not known during plan. Set it to a static value or use the "+EnvToken+" environment variable.")
	}
	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Unknown Prepaid-Host API endpoint",
			"The endpoint is not known during plan. Set it to a static value or use the "+EnvEndpoint+" environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	token := os.Getenv(EnvToken)
	if !cfg.Token.IsNull() {
		token = cfg.Token.ValueString()
	}
	endpoint := os.Getenv(EnvEndpoint)
	if !cfg.Endpoint.IsNull() {
		endpoint = cfg.Endpoint.ValueString()
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Missing Prepaid-Host API token",
			"Set the provider attribute \"token\" or the "+EnvToken+" environment variable to a personal API token from the customer panel.")
	}
	if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid Prepaid-Host API endpoint",
			"The endpoint must be an http(s) URL such as "+DefaultEndpoint+", got \""+endpoint+"\".")
	} else if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		resp.Diagnostics.AddAttributeWarning(path.Root("endpoint"), "Unencrypted Prepaid-Host API endpoint",
			"The endpoint \""+endpoint+"\" uses plain http, so the API token is sent unencrypted. Use https.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	opts := append([]client.Option{client.WithUserAgent("terraform-provider-prepaidhost/" + p.version)}, p.clientOptions...)
	c := client.New(endpoint, token, opts...)
	resp.DataSourceData = c
	resp.ResourceData = c
}

// Resources lists the managed resource types.
func (p *PrepaidHostProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewDomainContactResource,
		NewSSHKeyResource,
		NewVserverResource,
	}
}

// DataSources lists the data sources.
func (p *PrepaidHostProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewBalanceDataSource,
		NewCatalogDataSource,
	}
}

// isLoopback reports whether host is localhost or a loopback IP (plain http is fine there).
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
