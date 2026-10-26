// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ resource.ResourceWithConfigure        = &vserverResource{}
	_ resource.ResourceWithImportState      = &vserverResource{}
	_ resource.ResourceWithModifyPlan       = &vserverResource{}
	_ resource.ResourceWithConfigValidators = &vserverResource{}
)

// NewVserverResource returns the prepaidhost_vserver resource.
func NewVserverResource() resource.Resource { return &vserverResource{} }

type vserverResource struct {
	client *client.Client
}

const (
	statusActive     = "active"
	statusInstalling = "installing"
	// statusProvisioning: the order item of a taken-over domain is not active yet.
	statusProvisioning = "provisioning"
	statusExpired      = "expired"
	// minPostPurchase is the time the reads after a paid order get at least,
	// even when create_timeout is (nearly) used up.
	minPostPurchase = 30 * time.Second
	// Display name lengths the API accepts.
	minDisplayName = 4
	maxDisplayName = 32
)

// hostnamePattern is the API's hostname rule.
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

type vserverModel struct {
	ID          types.String `tfsdk:"id"`
	Hostname    types.String `tfsdk:"hostname"`
	DisplayName types.String `tfsdk:"display_name"`

	PacketID        types.Int64  `tfsdk:"packet_id"`
	DurationMonths  types.Int64  `tfsdk:"duration_months"`
	PacketPromoCode types.String `tfsdk:"packet_promo_code"`

	Cores        types.Int64  `tfsdk:"cores"`
	Memory       types.Int64  `tfsdk:"memory"`
	Disk         types.Int64  `tfsdk:"disk"`
	Addresses    types.Int64  `tfsdk:"addresses"`
	Backups      types.Int64  `tfsdk:"backups"`
	Traffic      types.String `tfsdk:"traffic"`
	DurationDays types.Int64  `tfsdk:"duration_days"`

	TemplateID    types.Int64   `tfsdk:"template_id"`
	SSHKeyIDs     types.Set     `tfsdk:"ssh_key_ids"`
	Password      types.String  `tfsdk:"password"`
	PromoCode     types.String  `tfsdk:"promo_code"`
	MaxPrice      types.Float64 `tfsdk:"max_price"`
	AutoRenew     types.Bool    `tfsdk:"auto_renew"`
	CreateTimeout types.String  `tfsdk:"create_timeout"`

	IPv4Addresses types.List   `tfsdk:"ipv4_addresses"`
	Status        types.String `tfsdk:"status"`
	ExpiresAt     types.String `tfsdk:"expires_at"`
}

// purchaseAttributes decide what was bought. They are never changed in place
// and never replace the server (that would buy a new one silently): a change
// is a plan error (see ModifyPlan).
//
// hostname belongs here too: it is the order's client reference and part of
// the Idempotency-Key, so it identifies the purchased server.
var purchaseAttributes = []string{
	"hostname", "packet_id", "duration_months", "cores", "memory", "disk", "addresses", "backups", "traffic", "duration_days", "template_id",
}

func (m *vserverModel) purchaseValues() map[string]attr.Value {
	return map[string]attr.Value{
		"hostname": m.Hostname, "packet_id": m.PacketID, "duration_months": m.DurationMonths,
		"cores": m.Cores, "memory": m.Memory, "disk": m.Disk, "addresses": m.Addresses, "backups": m.Backups,
		"traffic": m.Traffic, "duration_days": m.DurationDays, "template_id": m.TemplateID,
	}
}

// ---- API shapes ----

// flexBool decodes the MySQL tinyint the API returns for flags (0/1, or a bool/string).
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*f = false
	case bool:
		*f = flexBool(x)
	case float64:
		*f = flexBool(x != 0)
	case string:
		*f = flexBool(x != "" && x != "0" && !strings.EqualFold(x, "false"))
	default:
		return fmt.Errorf("expected a flag, got %s", b)
	}
	return nil
}

// vserverShowAPI is GET /rootserver/{id} (it answers exactly these fields;
// no hostname, IPs or expiry).
type vserverShowAPI struct {
	ID           flexInt  `json:"id"`
	Cores        flexInt  `json:"cores"`
	Memory       flexInt  `json:"memory"`
	DiskSize     flexInt  `json:"disk_size"`
	Addresses    flexInt  `json:"addresses"`
	Installed    flexBool `json:"installed"`
	BackupAmount flexInt  `json:"backup_amount"`
	TemplateID   flexInt  `json:"template_id"`
	PacketID     flexInt  `json:"packet_id"`
}

// vserverRentalAPI is GET /rootserver/{id}/rental (no envelope).
type vserverRentalAPI struct {
	CreatedRental *string `json:"created_rental"`
	DeleteRental  *string `json:"delete_rental"`
}

// vserverAddressAPI is one entry of GET /rootserver/{id}/addresses (no envelope).
type vserverAddressAPI struct {
	Address string `json:"address"`
}

// vserverRenewAPI is the part of GET /rootserver/{id}/renew the resource uses.
type vserverRenewAPI struct {
	AutoRenew bool `json:"auto_renew"`
}

// ---- schema ----

func (r *vserverResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vserver"
}

func purchaseNote(what string) string {
	return what + " Part of the purchase: changing it later is a plan error, never a new purchase."
}

func conflictsWithPacket() []validator.Int64 {
	return []validator.Int64{int64validator.ConflictsWith(path.MatchRoot("packet_id"))}
}

func (r *vserverResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A vServer (rootserver), bought from the prepaid balance. Either a root packet " +
			"(`packet_id` + `duration_months`) or a custom server (`cores`, `memory`, `disk`, `addresses`, `backups`, " +
			"`traffic`, `duration_days`) — see the `prepaidhost_catalog` data source for the values.\n\n" +
			"**Creating the resource spends money.** Create quotes the order, checks `max_price`, orders with a " +
			"deterministic Idempotency-Key and waits until the server is installed. An apply repeated after an abort " +
			"(even in a new process) gets the paid order back instead of buying again; a live server with the same " +
			"`hostname` is taken over instead of buying a second one (with a warning, and auto-renew switched on again).\n\n" +
			"**Destroying the resource cancels, it does not delete:** auto-renew is switched off and the server keeps " +
			"running until `expires_at`. There is no refund for the remaining term. Applying the same configuration " +
			"(same `hostname`) before that date takes the server over again without a new purchase.\n\n" +
			"The purchase arguments (`hostname`, `packet_id`, `duration_months`, `cores`, `memory`, `disk`, `addresses`, " +
			"`backups`, `traffic`, `duration_days`, `template_id`) are never changed in place and never replace the server: " +
			"a change is a plan error. Only `display_name` and `auto_renew` are updated in place.\n\n" +
			"A server deleted outside Terraform is removed from the state; the next apply buys a new one. A server whose " +
			"term expired stays in the state with `status = \"expired\"` as long as the API still lists it: with auto-renew " +
			"on, the platform may still renew it for about a day after expiry, and it is deleted a few days later. Renew it " +
			"in the panel, or destroy it to stop renewal.\n\n" +
			"`create_before_destroy` or `taint` with an unchanged configuration takes the same server over again (no " +
			"purchase); the delete of the deposed object then switches auto-renew off, which the next plan shows as a " +
			"one-time change of `auto_renew` from false to true.\n\n" +
			"Rarely, a server whose rental record was removed on the platform makes reads fail with HTTP 500; remove it " +
			"with `terraform state rm` then.\n\n" +
			"Needs a token with the scopes `rootserver:order` (ordering), `rootserver:read`, `rootserver:network:read` " +
			"(addresses) and `rootserver:renew` (auto-renew).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "vServer id.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"hostname": schema.StringAttribute{
				MarkdownDescription: purchaseNote("Hostname of the server (1–63 letters, digits and hyphens, no dots). It is " +
					"the order's client reference and part of its Idempotency-Key, so it identifies the purchased server: while a " +
					"live server carries it, ordering the same hostname takes that server over instead of buying another. " +
					"To rename the server in the panel use `display_name`. The API cannot read it back; after `terraform import` " +
					"the configured value is taken as is."),
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(hostnamePattern, "must consist of letters, digits and inner hyphens"),
				},
			},
			"display_name": schema.StringAttribute{
				MarkdownDescription: "Name of the server in the panel (4–32 characters); changed in place. The API cannot read it " +
					"back. Without it, a newly bought server is named after `hostname` (which may be shorter than 4 or longer than " +
					"32 characters — copied into the configuration it then fails this validation, so choose a valid name instead), " +
					"and for a server that was taken over or imported it stays unknown (null).",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.LengthBetween(minDisplayName, maxDisplayName)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"packet_id": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Root packet to buy (`prepaidhost_catalog` type `rootpacket`). Conflicts with the custom arguments."),
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"duration_months": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Term of a root packet in months: 1, 3, 6 or 12. Requires `packet_id`."),
				Optional:            true,
				Validators: []validator.Int64{
					int64validator.OneOf(1, 3, 6, 12),
					int64validator.AlsoRequires(path.MatchRoot("packet_id")),
				},
			},
			"packet_promo_code": schema.StringAttribute{
				MarkdownDescription: "Promotion keyword of a root packet (sent as the packet item's `promo_code`); without it " +
					"the packet's active promotion applies. Requires `packet_id`. Only used when buying.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 50),
					stringvalidator.AlsoRequires(path.MatchRoot("packet_id")),
				},
			},
			"cores": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("vCores of a custom server."),
				Optional:            true,
				Validators:          append(conflictsWithPacket(), int64validator.AtLeast(1)),
			},
			"memory": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Memory of a custom server in MB (catalog value, e.g. `2048`)."),
				Optional:            true,
				Validators:          append(conflictsWithPacket(), int64validator.AtLeast(1)),
			},
			"disk": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Disk of a custom server in GB."),
				Optional:            true,
				Validators:          append(conflictsWithPacket(), int64validator.AtLeast(1)),
			},
			"addresses": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Number of IPv4 addresses of a custom server."),
				Optional:            true,
				Validators:          append(conflictsWithPacket(), int64validator.AtLeast(1)),
			},
			"backups": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Backup slots of a custom server."),
				Optional:            true,
				Validators:          append(conflictsWithPacket(), int64validator.AtLeast(0)),
			},
			"traffic": schema.StringAttribute{
				MarkdownDescription: purchaseNote("Traffic option of a custom server (catalog value, e.g. `unlimited`). The API cannot read it back."),
				Optional:            true,
				Validators:          []validator.String{stringvalidator.ConflictsWith(path.MatchRoot("packet_id"))},
			},
			"duration_days": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Term of a custom server in days: 30, 90, 180 or 365 (rented as 1, 3, 6 or 12 months). The API cannot read it back."),
				Optional:            true,
				Validators:          append(conflictsWithPacket(), int64validator.OneOf(30, 90, 180, 365)),
			},
			"template_id": schema.Int64Attribute{
				MarkdownDescription: purchaseNote("Operating system template (`prepaidhost_catalog` → `templates`)."),
				Required:            true,
			},
			"ssh_key_ids": schema.SetAttribute{
				MarkdownDescription: "Ids of SSH keys of the account (`prepaidhost_ssh_key`) to install. Only used when buying; changing it later does not touch the server.",
				Optional:            true,
				ElementType:         types.Int64Type,
			},
			"password": schema.StringAttribute{
				MarkdownDescription: "Root/Administrator password (at least 6 characters; Windows templates have stricter rules). " +
					"Write-only: it is sent with the order and never stored in the state. Without it the API generates one. " +
					"Only used when buying. Needs Terraform 1.11+ or OpenTofu 1.11+.",
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(6),
				},
			},
			"promo_code": schema.StringAttribute{
				MarkdownDescription: "Promo code for the order (order-level `promo_code`). Only used when buying.",
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 50)},
			},
			"max_price": schema.Float64Attribute{
				MarkdownDescription: "Highest order total in EUR this apply may spend. A higher quote is an error before anything is ordered. Only used when buying.",
				Optional:            true,
				Validators:          []validator.Float64{float64validator.AtLeast(0)},
			},
			"auto_renew": schema.BoolAttribute{
				MarkdownDescription: "Renew the server automatically from the balance when its term ends. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"create_timeout": schema.StringAttribute{
				MarkdownDescription: "How long Create may take, including the installation (Go duration, e.g. `30m`, `1h`). Defaults to `30m`. " +
					"If it runs out after the order was paid, apply again: the same order is taken over, nothing is bought twice.",
				Optional:   true,
				Computed:   true,
				Default:    stringdefault.StaticString(defaultCreateTimeout.String()),
				Validators: []validator.String{durationValidator{}},
			},
			"ipv4_addresses": schema.ListAttribute{
				MarkdownDescription: "IPv4 addresses of the server.",
				Computed:            true,
				ElementType:         types.StringType,
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "`active` when installed, `installing` otherwise (e.g. during a reinstall), `expired` when the term ran out.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"expires_at": schema.StringAttribute{
				MarkdownDescription: "End of the paid term (RFC 3339, UTC). After `terraform destroy` the server runs until then.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *vserverResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	custom := []path.Expression{
		path.MatchRoot("cores"), path.MatchRoot("memory"), path.MatchRoot("disk"), path.MatchRoot("addresses"),
		path.MatchRoot("backups"), path.MatchRoot("traffic"), path.MatchRoot("duration_days"),
	}
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("packet_id"), path.MatchRoot("cores")),
		resourcevalidator.RequiredTogether(path.MatchRoot("packet_id"), path.MatchRoot("duration_months")),
		resourcevalidator.RequiredTogether(custom...),
	}
}

// durationValidator checks a positive Go duration.
type durationValidator struct{}

func (durationValidator) Description(context.Context) string {
	return "must be a positive duration such as 30m or 1h"
}
func (v durationValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (durationValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if d, err := time.ParseDuration(req.ConfigValue.ValueString()); err != nil || d <= 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid duration",
			fmt.Sprintf("%q is not a positive duration such as 30m or 1h.", req.ConfigValue.ValueString()))
	}
}

func (r *vserverResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (m *vserverModel) sshKeyIDs(ctx context.Context) ([]int64, diag.Diagnostics) {
	var ids []int64
	if m.SSHKeyIDs.IsNull() || m.SSHKeyIDs.IsUnknown() {
		return nil, nil
	}
	diags := m.SSHKeyIDs.ElementsAs(ctx, &ids, false)
	// Sorted: the API fingerprints the request body, so the same set must
	// always be sent in the same order.
	slices.Sort(ids)
	return ids, diags
}

// orderItem is the order item: exactly the fields the API evaluates for the item type
// (rootpacket when packet_id is set, else rootserver), unset ones left out.
// The engine adds client_reference (= hostname).
func (m *vserverModel) orderItem(ctx context.Context, password types.String) (map[string]any, diag.Diagnostics) {
	item := map[string]any{
		"hostname":    m.Hostname.ValueString(),
		"template_id": m.TemplateID.ValueInt64(),
	}
	if !m.PacketID.IsNull() {
		item["type"] = "rootpacket"
		item["packet_id"] = m.PacketID.ValueInt64()
		item["duration_months"] = m.DurationMonths.ValueInt64()
		if v := m.PacketPromoCode.ValueString(); v != "" {
			item["promo_code"] = v
		}
	} else {
		item["type"] = "rootserver"
		item["cores"] = m.Cores.ValueInt64()
		item["memory"] = m.Memory.ValueInt64()
		item["disk"] = m.Disk.ValueInt64()
		item["addresses"] = m.Addresses.ValueInt64()
		item["backups"] = m.Backups.ValueInt64()
		item["traffic"] = m.Traffic.ValueString()
		item["duration_days"] = m.DurationDays.ValueInt64()
	}
	if pw := password.ValueString(); pw != "" {
		item["password"] = pw
	}
	ids, diags := m.sshKeyIDs(ctx)
	if len(ids) > 0 {
		item["ssh_key_ids"] = ids
	}
	return item, diags
}

func (m *vserverModel) createTimeout() time.Duration {
	if d, err := time.ParseDuration(m.CreateTimeout.ValueString()); err == nil && d > 0 {
		return d
	}
	return defaultCreateTimeout
}

// mismatch lists how a server differs from the configured purchase.
func (m *vserverModel) mismatch(s *vserverShowAPI) []string {
	var out []string
	add := func(name string, got, want int64) {
		if got != want {
			out = append(out, fmt.Sprintf("%s is %d, configured %d", name, got, want))
		}
	}
	if !m.PacketID.IsNull() {
		add("packet_id", int64(s.PacketID), m.PacketID.ValueInt64())
	} else {
		if s.PacketID != 0 {
			out = append(out, fmt.Sprintf("it is root packet %d, configured a custom server", s.PacketID))
		}
		add("cores", int64(s.Cores), m.Cores.ValueInt64())
		add("memory", int64(s.Memory), m.Memory.ValueInt64())
		add("disk", int64(s.DiskSize), m.Disk.ValueInt64())
		add("addresses", int64(s.Addresses), m.Addresses.ValueInt64())
		add("backups", int64(s.BackupAmount), m.Backups.ValueInt64())
	}
	add("template_id", int64(s.TemplateID), m.TemplateID.ValueInt64())
	return out
}

// ---- API calls ----

func (r *vserverResource) show(ctx context.Context, id string) (*vserverShowAPI, error) {
	var s vserverShowAPI
	if err := r.client.Do(ctx, http.MethodGet, "/rootserver/"+id, nil, &s, nil); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *vserverResource) setAutoRenew(ctx context.Context, id string, enabled bool) error {
	return r.client.Do(ctx, http.MethodPut, "/rootserver/"+id+"/autorenew", map[string]any{"enabled": enabled}, nil, nil)
}

// expiry reads the end of the term from GET /rootserver/{id}/rental (nil if
// the API has none).
func (r *vserverResource) expiry(ctx context.Context, id string) (*time.Time, error) {
	var rental vserverRentalAPI
	if err := r.client.DoRaw(ctx, http.MethodGet, "/rootserver/"+id+"/rental", nil, &rental); err != nil {
		return nil, err
	}
	if rental.DeleteRental == nil {
		return nil, nil //nolint:nilnil // no expiry known
	}
	t, err := time.Parse(time.RFC3339, *rental.DeleteRental)
	if err != nil {
		return nil, fmt.Errorf("unreadable expiry %q from the API: %w", *rental.DeleteRental, err)
	}
	t = t.UTC()
	return &t, nil
}

func (r *vserverResource) ipv4Addresses(ctx context.Context, id string) (types.List, error) {
	var list []vserverAddressAPI
	if err := r.client.DoRaw(ctx, http.MethodGet, "/rootserver/"+id+"/addresses", nil, &list); err != nil {
		return types.ListNull(types.StringType), err
	}
	values := []attr.Value{}
	for _, a := range list {
		if ip, err := netip.ParseAddr(strings.TrimSpace(a.Address)); err == nil && ip.Is4() {
			values = append(values, types.StringValue(ip.String()))
		}
	}
	return types.ListValueMust(types.StringType, values), nil
}

// waitInstalled polls GET /rootserver/{id} until the server is installed or
// ctx ends (it then returns the last answer and ctx's error).
func (r *vserverResource) waitInstalled(ctx context.Context, id string) (*vserverShowAPI, error) {
	for {
		s, err := r.show(ctx, id)
		if err != nil {
			return nil, err
		}
		if s.Installed {
			return s, nil
		}
		t := time.NewTimer(r.client.PollInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return s, ctx.Err()
		case <-t.C:
		}
	}
}

// applyShow copies what GET /rootserver/{id} knows into the model. The mode
// follows the server: a packet sets packet_id and leaves the custom
// arguments null; a custom server sets them and leaves packet_id null. What
// the API cannot read back (hostname, traffic, the durations, the promo
// codes, ssh_key_ids) stays as it is.
func (m *vserverModel) applyShow(s *vserverShowAPI) {
	m.TemplateID = types.Int64Value(int64(s.TemplateID))
	if s.PacketID != 0 {
		m.PacketID = types.Int64Value(int64(s.PacketID))
		m.Cores, m.Memory, m.Disk, m.Addresses, m.Backups = types.Int64Null(), types.Int64Null(), types.Int64Null(), types.Int64Null(), types.Int64Null()
		m.Traffic, m.DurationDays = types.StringNull(), types.Int64Null()
	} else {
		m.PacketID, m.DurationMonths, m.PacketPromoCode = types.Int64Null(), types.Int64Null(), types.StringNull()
		m.Cores = types.Int64Value(int64(s.Cores))
		m.Memory = types.Int64Value(int64(s.Memory))
		m.Disk = types.Int64Value(int64(s.DiskSize))
		m.Addresses = types.Int64Value(int64(s.Addresses))
		m.Backups = types.Int64Value(int64(s.BackupAmount))
	}
	m.Status = types.StringValue(statusInstalling)
	if s.Installed {
		m.Status = types.StringValue(statusActive)
	}
}

// fillComputed reads expires_at and the IPv4 addresses.
func (r *vserverResource) fillComputed(ctx context.Context, m *vserverModel) error {
	id := m.ID.ValueString()
	exp, err := r.expiry(ctx, id)
	if err != nil {
		return err
	}
	m.ExpiresAt = types.StringNull()
	if exp != nil {
		m.ExpiresAt = types.StringValue(exp.Format(time.RFC3339))
	}
	m.IPv4Addresses, err = r.ipv4Addresses(ctx, id)
	return err
}

// ---- CRUD ----

func (r *vserverResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan vserverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	var password types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("password"), &password)...)
	if resp.Diagnostics.HasError() {
		return
	}

	timeout := plan.createTimeout()
	deadline := time.Now().Add(timeout)
	item, diags := plan.orderItem(ctx, password)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var maxPrice *float64
	if !plan.MaxPrice.IsNull() {
		v := plan.MaxPrice.ValueFloat64()
		maxPrice = &v
	}
	hostname := plan.Hostname.ValueString()

	resourceID, outcome, diags := orderResource(ctx, r.client, item, hostname, maxPrice, timeout,
		orderOptions{PromoCode: plan.PromoCode.ValueString()})
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		// Nothing is saved, also when the order was paid: the next apply
		// gets the same order back (see orderResource).
		return
	}
	id := strconv.FormatInt(resourceID, 10)
	existed := outcome != orderNew

	// From here on the server is paid (or taken over): every error says so.
	// The installation wait ends at the deadline; the reads around it get at
	// least minPostPurchase, so a create_timeout used up by the order does
	// not turn a paid purchase into a confusing read error.
	paidError := func(summary, problem string, err error) {
		what := "is already paid"
		if outcome == orderReferenceInUse {
			what = "was taken over"
		}
		detail := fmt.Sprintf("vServer %s %s, but %s. Run apply again without changing hostname or the purchase "+
			"arguments — it will not be bought twice.", id, what, problem)
		if err != nil {
			detail += "\n\n" + describeError(err)
		}
		resp.Diagnostics.AddError(summary, detail)
	}
	postCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, max(time.Until(deadline), minPostPurchase))
	}

	rctx, cancel := postCtx()
	defer cancel()
	s, err := r.show(rctx, id)
	if err != nil {
		paidError("Could not read the vServer", "it could not be read", err)
		return
	}
	if existed {
		// A server that existed before this apply: it must be the configured one.
		if diff := plan.mismatch(s); len(diff) > 0 {
			resp.Diagnostics.AddAttributeError(path.Root("hostname"), "Hostname belongs to another vServer",
				fmt.Sprintf("The hostname %q is the client reference of the live vServer %s, which differs from the configuration: %s. "+
					"Nothing was bought, and the server was not changed. Use another hostname, or set the configuration to that "+
					"server and apply again to manage it.", hostname, id, strings.Join(diff, "; ")))
			return
		}
	}

	// A new server renews by default; one that existed may have auto-renew
	// off (destroy switched it off).
	wantRenew := plan.AutoRenew.ValueBool()
	if existed || !wantRenew {
		if err := r.setAutoRenew(rctx, id, wantRenew); err != nil {
			paidError("Could not set auto-renew of the vServer", "auto-renew could not be set", err)
			return
		}
	}
	if existed {
		resp.Diagnostics.AddWarning("Existing vServer taken over",
			fmt.Sprintf("vServer %s with the hostname %q existed already: an earlier order with this configuration was "+
				"reused (an interrupted apply, or apply after destroy), or a live server carries this hostname. "+
				"It is now managed by this resource. Auto-renew is %s.", id, hostname, onOff(wantRenew)))
	}

	if !s.Installed {
		wctx, cancelWait := context.WithDeadline(ctx, deadline)
		s, err = r.waitInstalled(wctx, id)
		cancelWait()
		if err != nil {
			paidError("vServer not installed",
				fmt.Sprintf("it is not installed after %s (raise create_timeout if the installation takes longer)", timeout), err)
			return
		}
	}

	fctx, cancelFill := postCtx()
	defer cancelFill()
	// The fulfiller names a new server after the hostname; a configured
	// display_name is set on top. A server that existed keeps its panel name,
	// which the API cannot read: without a configured display_name it stays
	// unknown (null) rather than claimed to be the hostname.
	switch {
	case plan.DisplayName.IsUnknown() || plan.DisplayName.IsNull():
		plan.DisplayName = types.StringValue(hostname)
		if existed {
			plan.DisplayName = types.StringNull()
		}
	case plan.DisplayName.ValueString() != hostname || existed:
		if err := r.setDisplayName(fctx, id, plan.DisplayName.ValueString()); err != nil {
			paidError("Could not set the display name", "its display name could not be set", err)
			return
		}
	}

	plan.ID = types.StringValue(id)
	plan.applyShow(s)
	if err := r.fillComputed(fctx, &plan); err != nil {
		paidError("Could not read the vServer", "its term and addresses could not be read", err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (r *vserverResource) setDisplayName(ctx context.Context, id, name string) error {
	return r.client.Do(ctx, http.MethodPut, "/rootserver/"+id+"/display-name", map[string]any{"display_name": name}, nil, nil)
}

// renewGrace is how long after expire_at the platform's auto-renew cron
// still renews a server (vServerAutoRenew: expire_at > now - 1 day).
const renewGrace = 24 * time.Hour

func (r *vserverResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state vserverModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	gone := func() {
		resp.Diagnostics.AddWarning("vServer no longer exists",
			fmt.Sprintf("vServer %s was not found in the account (deleted outside Terraform) and is removed from the state. "+
				"The next apply buys a new one.", id))
		resp.State.RemoveResource(ctx)
	}

	s, err := r.show(ctx, id)
	if client.IsNotFound(err) {
		gone()
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the vServer", err)
		return
	}
	exp, err := r.expiry(ctx, id)
	if client.IsNotFound(err) {
		gone()
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the vServer's term", err)
		return
	}
	var renew vserverRenewAPI
	switch err := r.client.Do(ctx, http.MethodGet, "/rootserver/"+id+"/renew", nil, &renew, nil); {
	case err == nil:
		state.AutoRenew = types.BoolValue(renew.AutoRenew)
	case client.IsNotFound(err):
		// show and rental found it, renew is owner-only.
		resp.Diagnostics.AddError("Not the owner of the vServer",
			fmt.Sprintf("vServer %s is visible to this account, but you are not its owner (shared access cannot be managed by this "+
				"provider: renewal, auto-renew and the display name are owner-only). Remove it from the configuration and the state.\n\n%s",
				id, describeError(err)))
		return
	default:
		// The renewal info prices the renewal and can fail on its own; the
		// rest of the server is still readable.
		resp.Diagnostics.AddWarning("Could not read auto-renew",
			fmt.Sprintf("auto_renew of vServer %s keeps its last known value.\n\n%s", id, describeError(err)))
		if state.AutoRenew.IsNull() || state.AutoRenew.IsUnknown() {
			state.AutoRenew = types.BoolValue(true)
		}
	}

	// An expired server stays in the state while the API lists it: the
	// platform may still renew it (and charge) shortly after expiry, so
	// dropping it here could lead to a new purchase next to a late renewal.
	// The warning follows auto-renew as just read.
	expired := exp != nil && exp.Before(time.Now())
	if expired {
		detail := fmt.Sprintf("The term of vServer %s expired on %s. With auto-renew on, the platform may still renew it until about %s; "+
			"after that it is deleted within a few days. Renew it in the panel, or destroy it to stop renewal.",
			id, exp.Format(time.RFC3339), exp.Add(renewGrace).Format(time.RFC3339))
		if !state.AutoRenew.ValueBool() {
			detail = fmt.Sprintf("The term of vServer %s expired on %s. Auto-renew is off, so the platform does not renew it; it is "+
				"deleted within a few days. Renew it in the panel, or destroy it to remove it from Terraform.", id, exp.Format(time.RFC3339))
		}
		resp.Diagnostics.AddWarning("vServer expired", detail)
	}

	state.applyShow(s)
	if expired {
		state.Status = types.StringValue(statusExpired)
	}
	state.ExpiresAt = types.StringNull()
	if exp != nil {
		state.ExpiresAt = types.StringValue(exp.Format(time.RFC3339))
	}
	ips, err := r.ipv4Addresses(ctx, id)
	if err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not read the vServer's addresses", err)
		return
	}
	state.IPv4Addresses = ips
	if state.CreateTimeout.IsNull() {
		state.CreateTimeout = types.StringValue(defaultCreateTimeout.String())
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// ModifyPlan refuses changes of the purchase (they would need a new
// purchase, which the provider never makes silently) and announces what
// destroy means.
func (r *vserverResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() {
		return
	}
	var state vserverModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if req.Plan.Raw.IsNull() {
		resp.Diagnostics.AddWarning("Destroy cancels the vServer", cancelText(state.ID.ValueString(), state.ExpiresAt, "Destroying"))
		return
	}
	var plan vserverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	stateValues, planValues := state.purchaseValues(), plan.purchaseValues()
	var changed []string
	hostnameChanged := false
	for _, name := range purchaseAttributes {
		before, after := stateValues[name], planValues[name]
		if before.IsNull() {
			// Not known from the API (e.g. after import): the configuration is taken as is.
			continue
		}
		if after.IsUnknown() || !after.Equal(before) {
			changed = append(changed, fmt.Sprintf("%s: %s → %s", name, before, after))
			hostnameChanged = hostnameChanged || name == "hostname"
		}
	}
	if len(changed) > 0 {
		hint := ""
		if hostnameChanged {
			hint = "hostname identifies the purchased server (it is the order's client reference); to rename it in the panel " +
				"use display_name. "
		}
		resp.Diagnostics.AddError("Purchase arguments cannot be changed",
			fmt.Sprintf("vServer %s was bought with another configuration:\n  %s\n\n%s"+
				"Changing it would need a new purchase, and the provider never buys again silently. "+
				"Restore the configuration of the existing server. If it was changed outside Terraform (upgrade or reinstall "+
				"in the panel), set the configuration to the current values. To buy a different server, add a new resource "+
				"with another hostname (and destroy this one to cancel it).", state.ID.ValueString(), strings.Join(changed, "\n  "), hint))
	}
}

// cancelText says what cancelling means, depending on whether the term is
// still running.
func cancelText(id string, expiresAt types.String, verb string) string {
	if exp, err := time.Parse(time.RFC3339, expiresAt.ValueString()); err == nil && !exp.After(time.Now()) {
		return fmt.Sprintf("%s vServer %s switches auto-renew off. Its term already expired on %s, so the platform will not renew it "+
			"and deletes it after the grace period. There is no refund.", verb, id, expiresAt.ValueString())
	}
	return fmt.Sprintf("%s vServer %s switches auto-renew off; the server keeps running until %s. There is no refund for the "+
		"remaining term. Applying the same configuration (same hostname) before then takes it over again without a new purchase.",
		verb, id, expiryText(expiresAt))
}

func expiryText(v types.String) string {
	if v.IsNull() || v.IsUnknown() || v.ValueString() == "" {
		return "the end of its paid term"
	}
	return v.ValueString()
}

// Update changes only display_name and auto_renew on the server; everything
// else either cannot change (ModifyPlan) or is only used when buying and is
// just stored (also the purchase arguments filled in after an import).
func (r *vserverResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state vserverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	if plan.DisplayName.IsUnknown() {
		// Not configured and not known (after an import): stays unknown to Terraform.
		plan.DisplayName = state.DisplayName
	} else if !plan.DisplayName.Equal(state.DisplayName) {
		if err := r.setDisplayName(ctx, id, plan.DisplayName.ValueString()); err != nil {
			addAPIError(&resp.Diagnostics, "Could not rename the vServer", err)
			return
		}
	}
	if !plan.AutoRenew.Equal(state.AutoRenew) {
		if err := r.setAutoRenew(ctx, id, plan.AutoRenew.ValueBool()); err != nil {
			addAPIError(&resp.Diagnostics, "Could not set auto-renew of the vServer", err)
			return
		}
	}

	plan.ID, plan.IPv4Addresses, plan.Status, plan.ExpiresAt = state.ID, state.IPv4Addresses, state.Status, state.ExpiresAt
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete cancels: auto-renew off, the server runs until expires_at.
func (r *vserverResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vserverModel
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
		addAPIError(&resp.Diagnostics, "Could not cancel the vServer (switch auto-renew off)", err)
		return
	}
	expires := state.ExpiresAt
	if exp, err := r.expiry(ctx, id); err == nil && exp != nil {
		expires = types.StringValue(exp.Format(time.RFC3339))
	}
	resp.Diagnostics.AddWarning("vServer cancelled, not deleted", cancelText(id, expires, "Cancelling"))
}

func (r *vserverResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if n, err := strconv.ParseInt(req.ID, 10, 64); err != nil || n <= 0 {
		resp.Diagnostics.AddError("Invalid vServer id", fmt.Sprintf("The import id must be the numeric vServer id, got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
