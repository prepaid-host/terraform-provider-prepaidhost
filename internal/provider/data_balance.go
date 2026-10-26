// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ datasource.DataSource              = &balanceDataSource{}
	_ datasource.DataSourceWithConfigure = &balanceDataSource{}
)

// NewBalanceDataSource returns the prepaidhost_balance data source.
func NewBalanceDataSource() datasource.DataSource { return &balanceDataSource{} }

type balanceDataSource struct {
	client *client.Client
}

type balanceModel struct {
	Amount   types.Float64 `tfsdk:"amount"`
	Currency types.String  `tfsdk:"currency"`
}

func (d *balanceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_balance"
}

func (d *balanceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Current prepaid balance of the account (`GET /account/balance`). " +
			"Needs a token with the `account:billing` scope. Useful for a `check` or `precondition` before an order.",
		Attributes: map[string]schema.Attribute{
			"amount": schema.Float64Attribute{
				MarkdownDescription: "Balance in `currency`, rounded to cents.",
				Computed:            true,
			},
			"currency": schema.StringAttribute{
				MarkdownDescription: "Currency code, `EUR`.",
				Computed:            true,
			},
		},
	}
}

func (d *balanceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *client.Client, got %T.", req.ProviderData))
		return
	}
	d.client = c
}

func (d *balanceDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var out struct {
		Balance  float64 `json:"balance"`
		Currency string  `json:"currency"`
	}
	if err := d.client.Do(ctx, "GET", "/account/balance", nil, &out, nil); err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the account balance", err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, balanceModel{
		Amount:   types.Float64Value(out.Balance),
		Currency: types.StringValue(out.Currency),
	})...)
}
