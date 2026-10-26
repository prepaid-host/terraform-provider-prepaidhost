// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ resource.ResourceWithConfigure        = &dnsRecordResource{}
	_ resource.ResourceWithImportState      = &dnsRecordResource{}
	_ resource.ResourceWithConfigValidators = &dnsRecordResource{}
)

// NewDNSRecordResource returns the prepaidhost_dns_record resource.
func NewDNSRecordResource() resource.Resource { return &dnsRecordResource{} }

type dnsRecordResource struct {
	client *client.Client
}

// The API's record rules (DnsRecordService, StoreDnsRecordRequest).
var (
	dnsRecordTypes = []string{"A", "AAAA", "CNAME", "MX", "TXT", "SRV"}
	dnsTTLs        = []int64{60, 300, 600, 900, 1800, 3600, 21600, 43200, 86400, 172800, 259200, 604800, 1209600, 2592000}
	// noOuterSpace: the API trims the host, so surrounding blanks would be a permanent diff.
	noOuterSpace = regexp.MustCompile(`^\S(.*\S)?$`)
	// noUpperCase: DNS names are case-insensitive and the DNS provider may
	// store them in lower case; a mixed-case host would be a permanent diff.
	noUpperCase = regexp.MustCompile(`^[^A-Z]*$`)
)

const (
	defaultDNSTTL = 300
	// codeDNSRejected is DomainController::dnsError for a 422 (also an unknown record id).
	codeDNSRejected = "DNS_RECORD_REJECTED"
)

func hasPriority(typ string) bool { return typ == "MX" || typ == "SRV" }

type dnsRecordModel struct {
	ID       types.String `tfsdk:"id"`
	DomainID types.Int64  `tfsdk:"domain_id"`
	Type     types.String `tfsdk:"type"`
	Host     types.String `tfsdk:"host"`
	Record   types.String `tfsdk:"record"`
	TTL      types.Int64  `tfsdk:"ttl"`
	Priority types.Int64  `tfsdk:"priority"`
	Weight   types.Int64  `tfsdk:"weight"`
	Port     types.Int64  `tfsdk:"port"`
}

// dnsRecordAPI is one record as DnsRecordService::normalise presents it.
type dnsRecordAPI struct {
	ID       *flexInt `json:"id"`
	Type     string   `json:"type"`
	Host     string   `json:"host"`
	Record   string   `json:"record"`
	TTL      flexInt  `json:"ttl"`
	Priority *flexInt `json:"priority"`
	Weight   *flexInt `json:"weight"`
	Port     *flexInt `json:"port"`
}

func (a *dnsRecordAPI) id() int64 {
	if a.ID == nil {
		return 0
	}
	return int64(*a.ID)
}

func optionalInt(v *flexInt) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*v))
}

func (r *dnsRecordResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_record"
}

func (r *dnsRecordResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A DNS record in the zone of a domain of the account (`prepaidhost_domain`). The zone lives at the " +
			"platform's DNS provider; its apex NS records are read-only and cannot be managed.\n\n" +
			"A record deleted outside Terraform is removed from the state and created again by the next apply; a changed one " +
			"is set back. Changing `type` (or `domain_id`) replaces the record, everything else is changed in place.\n\n" +
			"Import with `domain_id/record_id`, e.g. `terraform import prepaidhost_dns_record.www 1234/567890` (the record ids " +
			"are listed by `GET /domain/{id}/dns`).\n\n" +
			"Needs a token with the scopes `domain:dns:read` and `domain:dns:write`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Record id at the DNS provider.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"domain_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the domain whose zone holds the record. Changing it replaces the record.",
				Required:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"type": schema.StringAttribute{
				MarkdownDescription: "Record type in upper case: `A`, `AAAA`, `CNAME`, `MX`, `TXT` or `SRV`. Changing it replaces " +
					"the record (the API cannot change a record's type).",
				Required:      true,
				Validators:    []validator.String{stringvalidator.OneOf(dnsRecordTypes...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"host": schema.StringAttribute{
				MarkdownDescription: "Host relative to the zone in lower case, e.g. `www` or `_dmarc`; `@` (the default) is the zone root.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("@"),
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 255),
					stringvalidator.RegexMatches(noOuterSpace, "must not start or end with blanks"),
					stringvalidator.RegexMatches(noUpperCase, "must be in lower case (DNS names are case-insensitive)"),
				},
			},
			"record": schema.StringAttribute{
				MarkdownDescription: "Value of the record (address, target host or text), at most 2048 characters. Target hosts of " +
					"`CNAME`, `MX` and `SRV` are written without trailing dot (`mail.example.com`). `TXT` values are sent verbatim.",
				Required:   true,
				Validators: []validator.String{stringvalidator.LengthBetween(1, 2048)},
			},
			"ttl": schema.Int64Attribute{
				MarkdownDescription: "TTL in seconds, one of 60, 300, 600, 900, 1800, 3600, 21600, 43200, 86400, 172800, 259200, " +
					"604800, 1209600, 2592000. Defaults to `300`.",
				Optional:   true,
				Computed:   true,
				Default:    int64default.StaticInt64(defaultDNSTTL),
				Validators: []validator.Int64{int64validator.OneOf(dnsTTLs...)},
			},
			"priority": schema.Int64Attribute{
				MarkdownDescription: "Priority (0–65535), only for `MX` and `SRV`. Without it the API uses 10 for MX and 0 for SRV. " +
					"Removing it from the configuration keeps the current value.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.Int64{int64validator.Between(0, 65535)},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"weight": schema.Int64Attribute{
				MarkdownDescription: "Weight (0–65535), required for `SRV` and only allowed there.",
				Optional:            true,
				Validators:          []validator.Int64{int64validator.Between(0, 65535)},
			},
			"port": schema.Int64Attribute{
				MarkdownDescription: "Port (1–65535), required for `SRV` and only allowed there.",
				Optional:            true,
				Validators:          []validator.Int64{int64validator.Between(1, 65535)},
			},
		},
	}
}

func (r *dnsRecordResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{dnsTypeFieldsValidator{}}
}

// dnsTypeFieldsValidator: priority only for MX/SRV, weight and port exactly for SRV.
type dnsTypeFieldsValidator struct{}

func (dnsTypeFieldsValidator) Description(context.Context) string {
	return "priority only for MX and SRV; weight and port required for SRV and only allowed there"
}
func (v dnsTypeFieldsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (dnsTypeFieldsValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m dnsRecordModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() || m.Type.IsUnknown() || m.Type.IsNull() {
		return
	}
	typ := m.Type.ValueString()
	if (typ == "CNAME" || typ == "MX" || typ == "SRV") && strings.HasSuffix(m.Record.ValueString(), ".") {
		resp.Diagnostics.AddAttributeError(path.Root("record"), "Trailing dot in record",
			fmt.Sprintf("Write the target host of a %s record without trailing dot (e.g. mail.example.com); the DNS provider "+
				"stores it in that form, so a trailing dot would show as a change on every plan.", typ))
	}
	if !hasPriority(typ) && !m.Priority.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("priority"), "priority not allowed",
			fmt.Sprintf("priority is only used with MX and SRV records, not with %s (the API would ignore it).", typ))
	}
	for _, f := range []struct {
		name string
		v    types.Int64
	}{{"weight", m.Weight}, {"port", m.Port}} {
		switch {
		case typ == "SRV" && f.v.IsNull():
			resp.Diagnostics.AddAttributeError(path.Root(f.name), "Missing "+f.name, "SRV records need weight and port.")
		case typ != "SRV" && !f.v.IsNull():
			resp.Diagnostics.AddAttributeError(path.Root(f.name), f.name+" not allowed",
				fmt.Sprintf("%s is only used with SRV records, not with %s.", f.name, typ))
		}
	}
}

func (r *dnsRecordResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *client.Client, got %T.", req.ProviderData))
		return
	}
	r.client = c
}

// body is the request for POST and PUT: the StoreDnsRecordRequest fields
// that apply to the type. PUT ignores type (it cannot change).
func (m *dnsRecordModel) body() map[string]any {
	typ := m.Type.ValueString()
	b := map[string]any{
		"type":   typ,
		"host":   m.Host.ValueString(),
		"record": m.Record.ValueString(),
		"ttl":    m.TTL.ValueInt64(),
	}
	if hasPriority(typ) && !m.Priority.IsNull() && !m.Priority.IsUnknown() {
		b["priority"] = m.Priority.ValueInt64()
	}
	if typ == "SRV" {
		b["weight"], b["port"] = m.Weight.ValueInt64(), m.Port.ValueInt64()
	}
	return b
}

func (m *dnsRecordModel) zonePath() string {
	return "/domain/" + strconv.FormatInt(m.DomainID.ValueInt64(), 10) + "/dns"
}

func (r *dnsRecordResource) list(ctx context.Context, m *dnsRecordModel) ([]dnsRecordAPI, error) {
	var recs []dnsRecordAPI
	err := r.client.Do(ctx, http.MethodGet, m.zonePath(), nil, &recs, nil)
	return recs, err
}

func find(recs []dnsRecordAPI, id int64) *dnsRecordAPI {
	for i := range recs {
		if recs[i].id() == id {
			return &recs[i]
		}
	}
	return nil
}

// applyAPI copies a record as read from the API into the model.
func (m *dnsRecordModel) applyAPI(rec *dnsRecordAPI) {
	m.ID = types.StringValue(strconv.FormatInt(rec.id(), 10))
	m.Type = types.StringValue(rec.Type)
	m.Host = types.StringValue(rec.Host)
	m.Record = types.StringValue(rec.Record)
	m.TTL = types.Int64Value(int64(rec.TTL))
	m.Priority = types.Int64Null()
	if hasPriority(rec.Type) {
		m.Priority = optionalInt(rec.Priority)
	}
	if rec.Type == "SRV" {
		if rec.Weight != nil {
			m.Weight = optionalInt(rec.Weight)
		}
		if rec.Port != nil {
			m.Port = optionalInt(rec.Port)
		}
	} else {
		m.Weight, m.Port = types.Int64Null(), types.Int64Null()
	}
}

// settle takes what the API decided (id, priority) and keeps the planned
// values of everything else, so an apply never reports an inconsistent result.
func (m *dnsRecordModel) settle(rec *dnsRecordAPI) {
	m.ID = types.StringValue(strconv.FormatInt(rec.id(), 10))
	if !hasPriority(m.Type.ValueString()) {
		m.Priority = types.Int64Null()
	} else if m.Priority.IsUnknown() || m.Priority.IsNull() {
		m.Priority = optionalInt(rec.Priority)
	}
}

func (r *dnsRecordResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dnsRecordModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var rec dnsRecordAPI
	if err := r.client.Do(ctx, http.MethodPost, plan.zonePath(), plan.body(), &rec, nil); err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the DNS record", err)
		return
	}
	if rec.id() == 0 {
		// The API falls back to the sent values when the DNS provider did not
		// name the new id: look it up in the zone.
		found, err := r.findCreated(ctx, &plan)
		if err != nil || found == nil {
			detail := "The DNS record was created, but the API did not return its id and it could not be found in the zone. " +
				"Look it up with GET /domain/{id}/dns and import it (domain_id/record_id) instead of applying again."
			if err != nil {
				detail += "\n\n" + describeError(err)
			}
			resp.Diagnostics.AddError("DNS record id unknown", detail)
			return
		}
		rec = *found
	}
	plan.settle(&rec)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// findCreated finds the newest record with the planned type, host and value.
func (r *dnsRecordResource) findCreated(ctx context.Context, m *dnsRecordModel) (*dnsRecordAPI, error) {
	recs, err := r.list(ctx, m)
	if err != nil {
		return nil, err
	}
	var best *dnsRecordAPI
	for i := range recs {
		c := &recs[i]
		if c.Type == m.Type.ValueString() && c.Host == m.Host.ValueString() && c.Record == m.Record.ValueString() &&
			(best == nil || c.id() > best.id()) {
			best = c
		}
	}
	return best, nil
}

func (r *dnsRecordResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dnsRecordModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid DNS record id", fmt.Sprintf("The state holds the record id %q, which is not numeric.", state.ID.ValueString()))
		return
	}

	recs, err := r.list(ctx, &state)
	if client.IsNotFound(err) {
		resp.Diagnostics.AddWarning("Domain no longer exists",
			fmt.Sprintf("Domain %d of DNS record %d is not in the account any more; the record is removed from the state.", state.DomainID.ValueInt64(), id))
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the DNS zone", err)
		return
	}
	rec := find(recs, id)
	if rec == nil {
		resp.Diagnostics.AddWarning("DNS record no longer exists",
			fmt.Sprintf("DNS record %d was not found in the zone of domain %d (deleted outside Terraform) and is removed from the state. "+
				"The next apply creates it again.", id, state.DomainID.ValueInt64()))
		resp.State.RemoveResource(ctx)
		return
	}
	if !slices.Contains(dnsRecordTypes, rec.Type) {
		resp.Diagnostics.AddError("Unsupported DNS record",
			fmt.Sprintf("DNS record %d is of type %s. %s records cannot be managed by this provider (the apex NS records belong to the "+
				"platform's nameservers); supported are %s.", id, rec.Type, rec.Type, strings.Join(dnsRecordTypes, ", ")))
		return
	}
	state.applyAPI(rec)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dnsRecordResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state dnsRecordModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := plan.body()
	delete(body, "type")
	var rec dnsRecordAPI
	if err := r.client.Do(ctx, http.MethodPut, plan.zonePath()+"/"+state.ID.ValueString(), body, &rec, nil); err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the DNS record", err)
		return
	}
	if rec.id() == 0 {
		id, _ := strconv.ParseInt(state.ID.ValueString(), 10, 64)
		rec.ID = (*flexInt)(&id)
	}
	plan.settle(&rec)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsRecordResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dnsRecordModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.Do(ctx, http.MethodDelete, state.zonePath()+"/"+state.ID.ValueString(), nil, nil, nil)
	switch {
	case err == nil, client.IsNotFound(err):
		return
	case client.IsCode(err, codeDNSRejected):
		// The API answers an unknown record id with a 422 as well: gone is fine.
		id, _ := strconv.ParseInt(state.ID.ValueString(), 10, 64)
		if recs, lerr := r.list(ctx, &state); client.IsNotFound(lerr) || (lerr == nil && find(recs, id) == nil) {
			return
		}
	}
	addAPIError(&resp.Diagnostics, "Could not delete the DNS record", err)
}

func (r *dnsRecordResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	domainPart, recordPart, ok := strings.Cut(req.ID, "/")
	domainID, derr := strconv.ParseInt(domainPart, 10, 64)
	recordID, rerr := strconv.ParseInt(recordPart, 10, 64)
	if !ok || derr != nil || rerr != nil || domainID <= 0 || recordID <= 0 {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("The import id must be domain_id/record_id with numeric ids, e.g. 1234/567890; got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain_id"), domainID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), strconv.FormatInt(recordID, 10))...)
}
