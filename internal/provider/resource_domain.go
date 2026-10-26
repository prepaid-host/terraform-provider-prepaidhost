// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ resource.ResourceWithConfigure        = &domainResource{}
	_ resource.ResourceWithImportState      = &domainResource{}
	_ resource.ResourceWithModifyPlan       = &domainResource{}
	_ resource.ResourceWithConfigValidators = &domainResource{}
)

// NewDomainResource returns the prepaidhost_domain resource.
func NewDomainResource() resource.Resource { return &domainResource{} }

type domainResource struct {
	client *client.Client
}

const (
	actionRegister = "register"
	actionTransfer = "transfer"
)

var (
	// domainNamePattern: lower-case labels of letters, digits and inner
	// hyphens, at least two of them (sld + TLD). Punycode (xn--) is fine.
	domainNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	// nameserverPattern is SetNameserversRequest's hostname rule, in the
	// normalised form the API stores (lower case, no trailing dot).
	nameserverPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

type domainModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	DurationMonths types.Int64  `tfsdk:"duration_months"`
	Action         types.String `tfsdk:"action"`
	Authcode       types.String `tfsdk:"authcode"`
	ContactID      types.Int64  `tfsdk:"contact_id"`
	Nameservers    types.List   `tfsdk:"nameservers"`
	AutoRenew      types.Bool   `tfsdk:"auto_renew"`

	PromoCode     types.String  `tfsdk:"promo_code"`
	MaxPrice      types.Float64 `tfsdk:"max_price"`
	CreateTimeout types.String  `tfsdk:"create_timeout"`

	Status    types.String `tfsdk:"status"`
	ExpiresAt types.String `tfsdk:"expires_at"`
}

// domainPurchaseAttributes decide what was bought: never changed in place,
// never replaced (a change is a plan error, see ModifyPlan). name is the
// order's client reference, so it identifies the purchased domain.
var domainPurchaseAttributes = []string{"name", "duration_months", "action"}

func (m *domainModel) purchaseValues() map[string]attr.Value {
	return map[string]attr.Value{"name": m.Name, "duration_months": m.DurationMonths, "action": m.Action}
}

// splitDomainName splits a validated name into sld and TLD. The sld is a
// single label (DomainConfigurator: max 63, the panel's single-label rule),
// so everything after the first dot is the TLD — "shop.co.uk" is shop + co.uk.
// A sub-domain such as "www.example.de" therefore asks for the TLD
// "example.de", which the catalog does not have: the price quote refuses it
// before anything is ordered.
func splitDomainName(name string) (sld, tld string) {
	sld, tld, _ = strings.Cut(name, ".")
	return sld, tld
}

// ---- API shapes ----

// domainShowAPI is the part of GET /domain/{id} the resource reads. The
// answer also carries the auth code and the owner data; they are never
// decoded.
type domainShowAPI struct {
	ID  flexInt `json:"id"`
	Sld string  `json:"sld"`
	Tld string  `json:"tld"`
}

func (s *domainShowAPI) name() string { return s.Sld + "." + s.Tld }

// domainRenewAPI is the part of GET /domain/{id}/renew the resource uses.
type domainRenewAPI struct {
	ExpiresAt *string `json:"expires_at"`
	AutoRenew bool    `json:"auto_renew"`
}

type domainNameserversAPI struct {
	Nameservers []string `json:"nameservers"`
}

// ---- schema ----

func (r *domainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

func (r *domainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A domain, registered or transferred and paid from the prepaid balance. The TLD must be in the " +
			"`prepaidhost_catalog` data source (type `domain`); the owner is a `prepaidhost_domain_contact`.\n\n" +
			"**Creating the resource spends money.** Create quotes the order, checks `max_price`, orders with a " +
			"deterministic Idempotency-Key and waits until the registrar accepted the registration or transfer. An apply " +
			"repeated after an abort (even in a new process) gets the paid order back instead of buying again; a live domain " +
			"with the same `name` ordered through the API is taken over instead of buying it again (with a warning, and " +
			"auto-renew switched on again). A domain registered in the panel is brought in with `terraform import`.\n\n" +
			"**Destroying the resource cancels, it does not delete:** auto-renew is switched off and the domain stays " +
			"registered until `expires_at`; after that it is not renewed and the registration lapses. There is no refund " +
			"for the remaining term. Applying the same configuration before that date takes the domain over again without " +
			"a new purchase.\n\n" +
			"The purchase arguments (`name`, `duration_months`, `action`) are never changed in place and never replace the " +
			"domain: a change is a plan error. `nameservers` and `auto_renew` are updated in place.\n\n" +
			"A domain deleted outside Terraform is removed from the state. A domain whose term expired stays in the state " +
			"with `status = \"expired\"` as long as the API lists it: the platform does not renew it any more; renew it in the " +
			"panel while the registry allows it, or destroy it.\n\n" +
			"Needs a token with the scopes `orders:write` (ordering), `domain:read`, `domain:renew` (term and auto-renew) and " +
			"`domain:nameserver` (only when `nameservers` is set).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Domain id.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: purchaseNote("Fully qualified domain name in lower case without trailing dot, e.g. `example.de` " +
					"or `shop.co.uk` (punycode for internationalised names). The first label is the second-level domain, the rest " +
					"the TLD, which must be in the catalog. At most 64 characters: the name is the order's client reference and " +
					"part of its Idempotency-Key, so it identifies the purchased domain."),
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthAtMost(maxClientReference),
					stringvalidator.RegexMatches(domainNamePattern,
						"must be a lower-case domain name such as example.de: labels of letters, digits and inner hyphens, no trailing dot"),
				},
			},
			"duration_months": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Term in months: 12, 24 or 36. Defaults to `12`. The API cannot read it back."),
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(12),
				Validators:          []validator.Int64{int64validator.OneOf(12, 24, 36)},
			},
			"action": schema.StringAttribute{
				MarkdownDescription: purchaseNote("`register` (default; the API checks that the name is available) or `transfer` " +
					"(needs `authcode`). The API cannot read it back."),
				Optional:   true,
				Computed:   true,
				Default:    stringdefault.StaticString(actionRegister),
				Validators: []validator.String{stringvalidator.OneOf(actionRegister, actionTransfer)},
			},
			"authcode": schema.StringAttribute{
				MarkdownDescription: "Auth code (EPP code) from the current registrar; required with `action = \"transfer\"` and " +
					"not allowed otherwise. Write-only: it is sent with the order and never stored in the state, never part of " +
					"the Idempotency-Key and never logged. Only used when buying. Needs Terraform 1.11+ or OpenTofu 1.11+.",
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
			},
			"contact_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the domain contact (`prepaidhost_domain_contact`) that becomes the owner. Only used " +
					"when buying: changing it later does not change the registrant (a plan warning says so; change the owner in " +
					"the panel). The API cannot read it back.",
				Required:   true,
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"nameservers": schema.ListAttribute{
				MarkdownDescription: "2 to 4 nameservers (lower case, no trailing dot, no IP addresses), set at the registrar. " +
					"Without it the provider does not manage them and only reads them (a new domain uses the platform's " +
					"`ns1`–`ns4.prepaid-host.com`). Removing it from the configuration keeps the current nameservers; to go back " +
					"to the platform's, set those four explicitly.",
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Validators: []validator.List{
					listvalidator.SizeBetween(2, 4),
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(nameserverValidator{}),
				},
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"auto_renew": schema.BoolAttribute{
				MarkdownDescription: "Renew the domain automatically from the balance before its term ends. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"promo_code": schema.StringAttribute{
				MarkdownDescription: "Promo code for the order. Only used when buying.",
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 50)},
			},
			"max_price": schema.Float64Attribute{
				MarkdownDescription: "Highest order total in EUR this apply may spend. A higher quote is an error before anything is ordered. Only used when buying.",
				Optional:            true,
				Validators:          []validator.Float64{float64validator.AtLeast(0)},
			},
			"create_timeout": schema.StringAttribute{
				MarkdownDescription: "How long Create may take, including the registrar's confirmation (Go duration). Defaults to `30m`. " +
					"If it runs out after the order was paid, apply again: the same order is taken over, nothing is bought twice.",
				Optional:   true,
				Computed:   true,
				Default:    stringdefault.StaticString(defaultCreateTimeout.String()),
				Validators: []validator.String{durationValidator{}},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "`active`, or `expired` when the term ran out.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"expires_at": schema.StringAttribute{
				MarkdownDescription: "End of the paid term (RFC 3339, UTC). After `terraform destroy` the domain stays registered until then.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *domainResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{authcodeValidator{}}
}

// authcodeValidator: a transfer needs the auth code, a registration takes none.
type authcodeValidator struct{}

func (authcodeValidator) Description(context.Context) string {
	return `authcode is required with action = "transfer" and not allowed otherwise`
}
func (v authcodeValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (authcodeValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var action, authcode types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("action"), &action)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("authcode"), &authcode)...)
	if resp.Diagnostics.HasError() || action.IsUnknown() || authcode.IsUnknown() {
		return
	}
	transfer := action.ValueString() == actionTransfer
	switch {
	case transfer && authcode.ValueString() == "":
		resp.Diagnostics.AddAttributeError(path.Root("authcode"), "Missing authcode",
			`action = "transfer" needs the authcode (EPP code) from the current registrar.`)
	case !transfer && !authcode.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("authcode"), "Unexpected authcode",
			`authcode is only used with action = "transfer"; a registration takes none.`)
	}
}

// nameserverValidator checks one nameserver like SetNameserversRequest, in
// the normalised form the API stores (otherwise every plan would differ).
type nameserverValidator struct{}

func (nameserverValidator) Description(context.Context) string {
	return "must be a lower-case host name without trailing dot, not an IP address"
}
func (v nameserverValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (nameserverValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	v := req.ConfigValue.ValueString()
	if len(v) > 255 || !nameserverPattern.MatchString(v) || net.ParseIP(v) != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid nameserver",
			fmt.Sprintf("%q is not a nameserver host name: use lower case, no trailing dot and no IP address (e.g. ns1.example.com).", v))
	}
}

func (r *domainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ---- order item ----

// orderItem is exactly the API's domain INPUT_KEYS that are set; the engine
// adds client_reference (= name). The auth code goes only into a transfer.
func (m *domainModel) orderItem(authcode types.String) map[string]any {
	sld, tld := splitDomainName(m.Name.ValueString())
	item := map[string]any{
		"type":            "domain",
		"sld":             sld,
		"tld":             tld,
		"duration_months": m.DurationMonths.ValueInt64(),
		"action":          m.Action.ValueString(),
		"contact_id":      m.ContactID.ValueInt64(),
	}
	if m.Action.ValueString() == actionTransfer {
		item["authcode"] = authcode.ValueString()
	}
	return item
}

func (m *domainModel) createTimeout() time.Duration {
	if d, err := time.ParseDuration(m.CreateTimeout.ValueString()); err == nil && d > 0 {
		return d
	}
	return defaultCreateTimeout
}

// ---- API calls ----

func (r *domainResource) show(ctx context.Context, id string) (*domainShowAPI, error) {
	var s domainShowAPI
	if err := r.client.Do(ctx, http.MethodGet, "/domain/"+id, nil, &s, nil); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *domainResource) setAutoRenew(ctx context.Context, id string, enabled bool) error {
	return r.client.Do(ctx, http.MethodPut, "/domain/"+id+"/autorenew", map[string]any{"enabled": enabled}, nil, nil)
}

// term reads expires_at and auto_renew from GET /domain/{id}/renew.
func (r *domainResource) term(ctx context.Context, id string) (*time.Time, bool, error) {
	var info domainRenewAPI
	if err := r.client.Do(ctx, http.MethodGet, "/domain/"+id+"/renew", nil, &info, nil); err != nil {
		return nil, false, err
	}
	if info.ExpiresAt == nil {
		return nil, info.AutoRenew, nil
	}
	t, err := time.Parse(time.RFC3339, *info.ExpiresAt)
	if err != nil {
		return nil, false, fmt.Errorf("unreadable expiry %q from the API: %w", *info.ExpiresAt, err)
	}
	t = t.UTC()
	return &t, info.AutoRenew, nil
}

// normaliseNameserver is the form the API stores: lower case, no trailing dot.
func normaliseNameserver(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

func nameserverList(list []string) types.List {
	values := make([]attr.Value, 0, len(list))
	for _, n := range list {
		values = append(values, types.StringValue(normaliseNameserver(n)))
	}
	return types.ListValueMust(types.StringType, values)
}

func (r *domainResource) nameservers(ctx context.Context, id string) (types.List, error) {
	var ns domainNameserversAPI
	if err := r.client.Do(ctx, http.MethodGet, "/domain/"+id+"/nameservers", nil, &ns, nil); err != nil {
		return types.ListNull(types.StringType), err
	}
	return nameserverList(ns.Nameservers), nil
}

func (r *domainResource) setNameservers(ctx context.Context, id string, want types.List) (types.List, error) {
	var list []string
	if diags := want.ElementsAs(ctx, &list, false); diags.HasError() {
		return want, fmt.Errorf("reading nameservers: %v", diags)
	}
	var ns domainNameserversAPI
	if err := r.client.Do(ctx, http.MethodPut, "/domain/"+id+"/nameservers", map[string]any{"nameservers": list}, &ns, nil); err != nil {
		return want, err
	}
	return nameserverList(ns.Nameservers), nil
}

// nameserverErrorDetail explains a failed nameserver change; state_unknown
// means the registrar may have applied it anyway.
func nameserverErrorDetail(err error) string {
	detail := describeError(err)
	if apiErr, ok := client.AsAPIError(err); ok {
		if unknown, _ := apiErr.Extra["state_unknown"].(bool); unknown {
			detail = "The registrar did not confirm the change: it may already be applied or still be underway. Run apply again " +
				"later: it reads the nameservers first and only sends them again if they still differ.\n\n" + detail
		}
	}
	return detail
}

func (m *domainModel) setTerm(exp *time.Time) {
	m.ExpiresAt = types.StringNull()
	m.Status = types.StringValue(statusActive)
	if exp != nil {
		m.ExpiresAt = types.StringValue(exp.Format(time.RFC3339))
		if exp.Before(time.Now()) {
			m.Status = types.StringValue(statusExpired)
		}
	}
}

// ---- CRUD ----

func (r *domainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	var authcode types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("authcode"), &authcode)...)
	if resp.Diagnostics.HasError() {
		return
	}

	timeout := plan.createTimeout()
	deadline := time.Now().Add(timeout)
	var maxPrice *float64
	if !plan.MaxPrice.IsNull() {
		v := plan.MaxPrice.ValueFloat64()
		maxPrice = &v
	}
	name := plan.Name.ValueString()

	resourceID, outcome, diags := orderResource(ctx, r.client, plan.orderItem(authcode), name, maxPrice, timeout,
		orderOptions{PromoCode: plan.PromoCode.ValueString()})
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		// Nothing is saved, also when the order was paid: the next apply
		// gets the same order back (see orderResource).
		return
	}
	id := strconv.FormatInt(resourceID, 10)
	existed := outcome != orderNew

	// From here on the domain is paid (or taken over): every error says so,
	// and the reads get at least minPostPurchase (see the vServer).
	paidError := func(summary, problem string, detail string) {
		what := "is already paid"
		if outcome == orderReferenceInUse {
			what = "was taken over"
		}
		text := fmt.Sprintf("Domain %s (%s) %s, but %s. Run apply again without changing name or the purchase arguments — "+
			"it will not be bought twice.", id, name, what, problem)
		if detail != "" {
			text += "\n\n" + detail
		}
		resp.Diagnostics.AddError(summary, text)
	}
	pctx, cancel := context.WithTimeout(ctx, max(time.Until(deadline), minPostPurchase))
	defer cancel()

	if _, err := r.show(pctx, id); err != nil {
		paidError("Could not read the domain", "it could not be read", describeError(err))
		return
	}
	wantRenew := plan.AutoRenew.ValueBool()
	if existed || !wantRenew {
		if err := r.setAutoRenew(pctx, id, wantRenew); err != nil {
			paidError("Could not set auto-renew of the domain", "auto-renew could not be set", describeError(err))
			return
		}
	}
	if existed {
		resp.Diagnostics.AddWarning("Existing domain taken over",
			fmt.Sprintf("Domain %s (%s) existed already: an earlier order with this configuration was reused (an interrupted "+
				"apply, or apply after destroy), or a live domain carries this name. It is now managed by this resource. "+
				"Auto-renew is %s.", id, name, onOff(wantRenew)))
	}

	current, err := r.nameservers(pctx, id)
	if err != nil {
		paidError("Could not read the nameservers", "its nameservers could not be read", describeError(err))
		return
	}
	if !plan.Nameservers.IsUnknown() && !plan.Nameservers.IsNull() && !plan.Nameservers.Equal(current) {
		if current, err = r.setNameservers(pctx, id, plan.Nameservers); err != nil {
			paidError("Could not set the nameservers", "its nameservers could not be set", nameserverErrorDetail(err))
			return
		}
	}
	plan.Nameservers = current

	exp, _, err := r.term(pctx, id)
	if err != nil {
		paidError("Could not read the domain's term", "its term could not be read", describeError(err))
		return
	}
	plan.ID = types.StringValue(id)
	plan.setTerm(exp)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	gone := func() {
		resp.Diagnostics.AddWarning("Domain no longer exists",
			fmt.Sprintf("Domain %s was not found in the account (deleted or moved outside Terraform) and is removed from the state.", id))
		resp.State.RemoveResource(ctx)
	}

	s, err := r.show(ctx, id)
	if client.IsNotFound(err) {
		gone()
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the domain", err)
		return
	}
	name := s.name()

	exp, autoRenew, err := r.term(ctx, id)
	switch {
	case err == nil:
		state.AutoRenew = types.BoolValue(autoRenew)
		state.setTerm(exp)
	case client.IsNotFound(err):
		// show found it (both are scoped to the owner): the rental is missing.
		resp.Diagnostics.AddError("Domain has no term",
			fmt.Sprintf("Domain %s (%s) is in the account, but has no term (rental) record, so it can be neither renewed nor "+
				"cancelled through the API. Contact support, or remove it from the configuration and with `terraform state rm`.\n\n%s",
				id, name, describeError(err)))
		return
	default:
		// The renewal info prices the renewal and can fail on its own.
		resp.Diagnostics.AddWarning("Could not read the domain's term",
			fmt.Sprintf("expires_at and auto_renew of domain %s keep their last known values.\n\n%s", id, describeError(err)))
		if state.AutoRenew.IsNull() || state.AutoRenew.IsUnknown() {
			state.AutoRenew = types.BoolValue(true)
		}
		if state.Status.IsNull() || state.Status.IsUnknown() {
			state.Status = types.StringValue(statusActive)
		}
	}
	if err == nil && exp != nil && exp.Before(time.Now()) {
		resp.Diagnostics.AddWarning("Domain expired",
			fmt.Sprintf("The term of domain %s (%s) expired on %s. The platform no longer renews it automatically. Renew it in the "+
				"panel while the registry still allows it, or destroy it to stop managing it.", id, name, exp.Format(time.RFC3339)))
	}

	ns, err := r.nameservers(ctx, id)
	if client.IsNotFound(err) {
		gone()
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the domain's nameservers", err)
		return
	}

	state.Name = types.StringValue(name)
	state.Nameservers = ns
	if state.CreateTimeout.IsNull() {
		state.CreateTimeout = types.StringValue(defaultCreateTimeout.String())
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// ModifyPlan refuses changes of the purchase, warns about a contact change
// (only used when buying) and announces what destroy means.
func (r *domainResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() {
		return
	}
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if req.Plan.Raw.IsNull() {
		resp.Diagnostics.AddWarning("Destroy cancels the domain", domainCancelText(state.ID.ValueString(), state.ExpiresAt, "Destroying"))
		return
	}
	var plan domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	stateValues, planValues := state.purchaseValues(), plan.purchaseValues()
	var changed []string
	nameChanged := false
	for _, name := range domainPurchaseAttributes {
		before, after := stateValues[name], planValues[name]
		if before.IsNull() {
			// Not known from the API (after import): the configuration is taken as is.
			continue
		}
		if after.IsUnknown() || !after.Equal(before) {
			changed = append(changed, fmt.Sprintf("%s: %s → %s", name, before, after))
			nameChanged = nameChanged || name == "name"
		}
	}
	if len(changed) > 0 {
		hint := ""
		if nameChanged {
			hint = "name identifies the purchased domain (it is the order's client reference). "
		}
		resp.Diagnostics.AddError("Purchase arguments cannot be changed",
			fmt.Sprintf("Domain %s was bought with another configuration:\n  %s\n\n%s"+
				"Changing it would need a new purchase, and the provider never buys again silently. Restore the configuration "+
				"of the existing domain. To buy another domain, add a new resource (and destroy this one to cancel it).",
				state.ID.ValueString(), strings.Join(changed, "\n  "), hint))
		return
	}

	if !state.ContactID.IsNull() && !plan.ContactID.IsUnknown() && !plan.ContactID.Equal(state.ContactID) {
		resp.Diagnostics.AddAttributeWarning(path.Root("contact_id"), "contact_id is only used when buying",
			fmt.Sprintf("Changing contact_id from %s to %s does not change the registrant of domain %s; the new value is only "+
				"stored. Change the owner in the customer panel.", state.ContactID, plan.ContactID, state.ID.ValueString()))
	}
}

// domainCancelText says what cancelling a domain means.
func domainCancelText(id string, expiresAt types.String, verb string) string {
	if exp, err := time.Parse(time.RFC3339, expiresAt.ValueString()); err == nil && !exp.After(time.Now()) {
		return fmt.Sprintf("%s domain %s switches auto-renew off. Its term already expired on %s, so it is not renewed. "+
			"There is no refund.", verb, id, expiresAt.ValueString())
	}
	return fmt.Sprintf("%s domain %s switches auto-renew off; the domain stays registered until %s and is not renewed after "+
		"that, so the registration lapses and the name can be lost. There is no refund for the remaining term. Applying the "+
		"same configuration (same name) before then takes it over again without a new purchase.", verb, id, expiryText(expiresAt))
}

// Update changes only the nameservers and auto_renew; everything else is
// either refused (ModifyPlan) or only used when buying and just stored.
func (r *domainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	if plan.Nameservers.IsUnknown() || plan.Nameservers.IsNull() {
		plan.Nameservers = state.Nameservers
	} else if !plan.Nameservers.Equal(state.Nameservers) {
		ns, err := r.setNameservers(ctx, id, plan.Nameservers)
		if err != nil {
			resp.Diagnostics.AddError("Could not set the nameservers", nameserverErrorDetail(err))
			return
		}
		plan.Nameservers = ns
	}
	if !plan.AutoRenew.Equal(state.AutoRenew) {
		if err := r.setAutoRenew(ctx, id, plan.AutoRenew.ValueBool()); err != nil {
			addAPIError(&resp.Diagnostics, "Could not set auto-renew of the domain", err)
			return
		}
	}

	plan.ID, plan.Status, plan.ExpiresAt = state.ID, state.Status, state.ExpiresAt
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete cancels: auto-renew off, the domain stays registered until expires_at.
func (r *domainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	err := r.setAutoRenew(ctx, id, false)
	if client.IsNotFound(err) {
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not cancel the domain (switch auto-renew off)", err)
		return
	}
	expires := state.ExpiresAt
	if exp, _, err := r.term(ctx, id); err == nil && exp != nil {
		expires = types.StringValue(exp.Format(time.RFC3339))
	}
	resp.Diagnostics.AddWarning("Domain cancelled, not deleted", domainCancelText(id, expires, "Cancelling"))
}

func (r *domainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if n, err := strconv.ParseInt(req.ID, 10, 64); err != nil || n <= 0 {
		resp.Diagnostics.AddError("Invalid domain id", fmt.Sprintf("The import id must be the numeric domain id, got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
