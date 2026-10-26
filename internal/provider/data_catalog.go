// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ datasource.DataSource              = &catalogDataSource{}
	_ datasource.DataSourceWithConfigure = &catalogDataSource{}
)

// catalogTypes are the product types GET /catalog/{type} knows.
var catalogTypes = []string{"rootserver", "rootpacket", "domain"}

// NewCatalogDataSource returns the prepaidhost_catalog data source.
func NewCatalogDataSource() datasource.DataSource { return &catalogDataSource{} }

type catalogDataSource struct {
	client *client.Client
}

// flexString decodes a JSON string or number into a string (option values and
// traffic are numbers for some entries and "unlimited" for others).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("expected string or number, got %s", b)
	}
	*f = flexString(n.String())
	return nil
}

// catalogModel is both the API result (json tags; the fields not used by a
// type stay nil) and the Terraform state (tfsdk tags; nil = null).
type catalogModel struct {
	Type types.String `tfsdk:"type" json:"-"`

	Options        *catalogOptions   `tfsdk:"options" json:"options"`
	Durations      []catalogDuration `tfsdk:"durations" json:"durations"`
	Packets        []catalogPacket   `tfsdk:"packets" json:"packets"`
	Tlds           []catalogTLD      `tfsdk:"tlds" json:"tlds"`
	DurationMonths []int64           `tfsdk:"durations_months" json:"durations_months"`
	Templates      []catalogTemplate `tfsdk:"templates" json:"templates"`
}

type catalogOptions struct {
	Cores     []catalogOption `tfsdk:"cores" json:"cores"`
	Memory    []catalogOption `tfsdk:"memory" json:"memory"`
	Disk      []catalogOption `tfsdk:"disk" json:"disk"`
	Addresses []catalogOption `tfsdk:"addresses" json:"addresses"`
	Backups   []catalogOption `tfsdk:"backups" json:"backups"`
	Traffic   []catalogOption `tfsdk:"traffic" json:"traffic"`
}

type catalogOption struct {
	Value        flexString `tfsdk:"value" json:"value"`
	Name         string     `tfsdk:"name" json:"name"`
	PriceMonthly float64    `tfsdk:"price_monthly" json:"price_monthly"`
}

type catalogDuration struct {
	Days            int64   `tfsdk:"days" json:"days"`
	Months          int64   `tfsdk:"months" json:"months"`
	DiscountPercent float64 `tfsdk:"discount_percent" json:"discount_percent"`
}

type catalogPacket struct {
	ID                 int64      `tfsdk:"id" json:"id"`
	Name               string     `tfsdk:"name" json:"name"`
	Cores              int64      `tfsdk:"cores" json:"cores"`
	Memory             int64      `tfsdk:"memory" json:"memory"`
	Disk               int64      `tfsdk:"disk" json:"disk"`
	Addresses          int64      `tfsdk:"addresses" json:"addresses"`
	Backups            int64      `tfsdk:"backups" json:"backups"`
	Traffic            flexString `tfsdk:"traffic" json:"traffic"`
	PriceMonthly       float64    `tfsdk:"price_monthly" json:"price_monthly"`
	ActivePromoPercent float64    `tfsdk:"active_promo_percent" json:"active_promo_percent"`
}

type catalogTLD struct {
	TLD                   string   `tfsdk:"tld" json:"tld"`
	PriceYearly           float64  `tfsdk:"price_yearly" json:"price_yearly"`
	PriceYearlyDiscounted *float64 `tfsdk:"price_yearly_discounted" json:"price_yearly_discounted"`
}

type catalogTemplate struct {
	ID       int64  `tfsdk:"id" json:"id"`
	Name     string `tfsdk:"name" json:"name"`
	OSFamily string `tfsdk:"os_family" json:"os_family"`
}

func (d *catalogDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_catalog"
}

func optionList(desc string) schema.ListNestedAttribute {
	return schema.ListNestedAttribute{
		MarkdownDescription: desc,
		Computed:            true,
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"value":         schema.StringAttribute{MarkdownDescription: "Value to pass in the order (for example `2048`).", Computed: true},
			"name":          schema.StringAttribute{MarkdownDescription: "Display name.", Computed: true},
			"price_monthly": schema.Float64Attribute{MarkdownDescription: "Monthly price of this option.", Computed: true},
		}},
	}
}

func (d *catalogDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Orderable packages, options and indicative prices of a product type (`GET /catalog/{type}`). " +
			"The API caches the catalog for up to five minutes; the binding price is always the quote of the order. " +
			"Which attributes are filled depends on `type`: `rootserver` fills `options`, `durations` and `templates`, " +
			"`rootpacket` fills `packets`, `durations_months` and `templates`, `domain` fills `tlds` and `durations_months`. " +
			"The others are null.",
		Attributes: map[string]schema.Attribute{
			"type": schema.StringAttribute{
				MarkdownDescription: "Product type: `rootserver`, `rootpacket` or `domain`.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.OneOf(catalogTypes...)},
			},
			"options": schema.SingleNestedAttribute{
				MarkdownDescription: "`rootserver` only: the selectable values per option.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"cores":     optionList("Selectable CPU core counts."),
					"memory":    optionList("Selectable memory sizes in MB."),
					"disk":      optionList("Selectable disk sizes in GB."),
					"addresses": optionList("Selectable numbers of IPv4 addresses."),
					"backups":   optionList("Selectable numbers of backup slots."),
					"traffic":   optionList("Selectable traffic options."),
				},
			},
			"durations": schema.ListNestedAttribute{
				MarkdownDescription: "`rootserver` only: rental durations with the current discount.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"days":             schema.Int64Attribute{MarkdownDescription: "Duration in days (`duration_days` of the order).", Computed: true},
					"months":           schema.Int64Attribute{MarkdownDescription: "Duration in months.", Computed: true},
					"discount_percent": schema.Float64Attribute{MarkdownDescription: "Discount in percent for this duration.", Computed: true},
				}},
			},
			"packets": schema.ListNestedAttribute{
				MarkdownDescription: "`rootpacket` only: the fixed packages.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":                   schema.Int64Attribute{MarkdownDescription: "Packet id (`packet_id` of the order).", Computed: true},
					"name":                 schema.StringAttribute{MarkdownDescription: "Packet name.", Computed: true},
					"cores":                schema.Int64Attribute{MarkdownDescription: "CPU cores.", Computed: true},
					"memory":               schema.Int64Attribute{MarkdownDescription: "Memory in MB.", Computed: true},
					"disk":                 schema.Int64Attribute{MarkdownDescription: "Disk size in GB.", Computed: true},
					"addresses":            schema.Int64Attribute{MarkdownDescription: "Number of IPv4 addresses.", Computed: true},
					"backups":              schema.Int64Attribute{MarkdownDescription: "Number of backup slots.", Computed: true},
					"traffic":              schema.StringAttribute{MarkdownDescription: "Traffic, `unlimited` or a number.", Computed: true},
					"price_monthly":        schema.Float64Attribute{MarkdownDescription: "Monthly price.", Computed: true},
					"active_promo_percent": schema.Float64Attribute{MarkdownDescription: "Percentage of the currently active promotion, `0` if none.", Computed: true},
				}},
			},
			"tlds": schema.ListNestedAttribute{
				MarkdownDescription: "`domain` only: the registrable top-level domains.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"tld":                     schema.StringAttribute{MarkdownDescription: "Top-level domain without dot, for example `com`.", Computed: true},
					"price_yearly":            schema.Float64Attribute{MarkdownDescription: "Price per year.", Computed: true},
					"price_yearly_discounted": schema.Float64Attribute{MarkdownDescription: "Discounted price per year; null if no discount is active.", Computed: true},
				}},
			},
			"durations_months": schema.ListAttribute{
				MarkdownDescription: "`rootpacket` and `domain`: the orderable durations in months.",
				Computed:            true,
				ElementType:         types.Int64Type,
			},
			"templates": schema.ListNestedAttribute{
				MarkdownDescription: "`rootserver` and `rootpacket`: the installable operating system templates.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":        schema.Int64Attribute{MarkdownDescription: "Template id (`template_id` of the order).", Computed: true},
					"name":      schema.StringAttribute{MarkdownDescription: "Operating system name.", Computed: true},
					"os_family": schema.StringAttribute{MarkdownDescription: "Operating system family, for example `linux`.", Computed: true},
				}},
			},
		},
	}
}

func (d *catalogDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *catalogDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg catalogModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var out catalogModel
	if err := d.client.Do(ctx, "GET", "/catalog/"+cfg.Type.ValueString(), nil, &out, nil); err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the catalog", err)
		return
	}
	out.Type = cfg.Type
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
