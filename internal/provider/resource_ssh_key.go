// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/md5" //nolint:gosec // the API's fingerprint format (UserSshKey::generateFingerprint), not a security use
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ resource.Resource                = &sshKeyResource{}
	_ resource.ResourceWithConfigure   = &sshKeyResource{}
	_ resource.ResourceWithImportState = &sshKeyResource{}
)

// NewSSHKeyResource returns the prepaidhost_ssh_key resource.
func NewSSHKeyResource() resource.Resource { return &sshKeyResource{} }

type sshKeyResource struct {
	client *client.Client
}

type sshKeyModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	PublicKey            types.String `tfsdk:"public_key"`
	ProvisionOnReinstall types.Bool   `tfsdk:"provision_on_reinstall"`
	Fingerprint          types.String `tfsdk:"fingerprint"`
	CreatedAt            types.String `tfsdk:"created_at"`
}

// sshKeyAPI is one entry of GET /account/ssh-keys (and the POST answer).
type sshKeyAPI struct {
	ID                   int64  `json:"id"`
	Name                 string `json:"name"`
	Fingerprint          string `json:"fingerprint"`
	ProvisionOnReinstall bool   `json:"provision_on_reinstall"`
	CreatedAt            string `json:"created_at"`
}

func (r *sshKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ssh_key"
}

func (r *sshKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A public SSH key stored in the account, which can be installed on vServers. " +
			"Storing keys is free. Needs a token with the `account:sshkeys` scope. " +
			"The API has no update, so changing any argument replaces the key.\n\n" +
			"Destroying the resource removes the key from the account only; keys already installed on servers stay there.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Key id in the account.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the key (max. 100 characters).",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"public_key": schema.StringAttribute{
				MarkdownDescription: "The public key in OpenSSH format, for example `ssh-ed25519 AAAA... user@host`. " +
					"The API never returns it, so it is not set after `terraform import`: the first apply with the configured key " +
					"fills it in (without replacing the key) if its fingerprint matches the imported key.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIf(
						// A null state value means "imported, key unknown": adopt it in place (see Update).
						func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
							resp.RequiresReplace = !req.StateValue.IsNull()
						},
						"Replaces the key when the public key changes, unless it was unknown after an import.",
						"Replaces the key when the public key changes, unless it was unknown after an import.",
					),
				},
			},
			"provision_on_reinstall": schema.BoolAttribute{
				MarkdownDescription: "Install this key automatically on every reinstall of a vServer. Defaults to `false`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"fingerprint": schema.StringAttribute{
				MarkdownDescription: "Fingerprint of the key as the API reports it (MD5, colon-separated hex pairs).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the key was stored (RFC 3339).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *sshKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// sshFingerprint is the API's fingerprint of a public key (MD5 of the decoded
// key, colon-separated hex), "" if the key cannot be decoded.
func sshFingerprint(publicKey string) string {
	parts := strings.Fields(publicKey)
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	sum := md5.Sum(raw) //nolint:gosec // see import
	h := hex.EncodeToString(sum[:])
	pairs := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":")
}

func (m *sshKeyModel) fromAPI(k sshKeyAPI) {
	m.ID = types.StringValue(strconv.FormatInt(k.ID, 10))
	m.Name = types.StringValue(k.Name)
	m.Fingerprint = types.StringValue(k.Fingerprint)
	m.ProvisionOnReinstall = types.BoolValue(k.ProvisionOnReinstall)
	m.CreatedAt = types.StringValue(k.CreatedAt)
}

func (r *sshKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sshKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		"name":                   plan.Name.ValueString(),
		"public_key":             plan.PublicKey.ValueString(),
		"provision_on_reinstall": plan.ProvisionOnReinstall.ValueBool(),
	}
	var created sshKeyAPI
	if err := r.client.Do(ctx, http.MethodPost, "/account/ssh-keys", body, &created, nil); err != nil {
		addAPIError(&resp.Diagnostics, "Could not store the SSH key", err)
		return
	}
	plan.fromAPI(created)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// find returns the key with the given id from GET /account/ssh-keys (the API
// has no read-by-id), nil if it is not in the list.
func (r *sshKeyResource) find(ctx context.Context, id string) (*sshKeyAPI, error) {
	var keys []sshKeyAPI
	if err := r.client.Do(ctx, http.MethodGet, "/account/ssh-keys", nil, &keys, nil); err != nil {
		return nil, err
	}
	for i := range keys {
		if strconv.FormatInt(keys[i].ID, 10) == id {
			return &keys[i], nil
		}
	}
	return nil, nil //nolint:nilnil // nil means "not in the list"
}

func (r *sshKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sshKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := r.find(ctx, state.ID.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the SSH keys", err)
		return
	}
	if key == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.fromAPI(*key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only runs to fill in the public key after an import (every other
// change replaces the key). It checks the key against the stored fingerprint.
func (r *sshKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state sshKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if got := sshFingerprint(plan.PublicKey.ValueString()); got == "" || got != state.Fingerprint.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("public_key"), "public_key does not match the stored key",
			fmt.Sprintf("The imported key %s has the fingerprint %s, but the configured public_key has the fingerprint %q. "+
				"Configure the public key that belongs to this key, or remove the key from the state and let Terraform create a new one.",
				state.ID.ValueString(), state.Fingerprint.ValueString(), got))
		return
	}
	plan.ID = state.ID
	plan.Fingerprint = state.Fingerprint
	plan.CreatedAt = state.CreatedAt
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sshKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sshKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Do(ctx, http.MethodDelete, "/account/ssh-keys/"+state.ID.ValueString(), nil, nil, nil)
	if err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not remove the SSH key", err)
	}
}

func (r *sshKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := strconv.ParseInt(req.ID, 10, 64); err != nil {
		resp.Diagnostics.AddError("Invalid SSH key id", fmt.Sprintf("The import id must be the numeric key id, got %q.", req.ID))
		return
	}
	key, err := r.find(ctx, req.ID)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the SSH keys", err)
		return
	}
	if key == nil {
		resp.Diagnostics.AddError("SSH key not found", fmt.Sprintf("No SSH key with id %s was found in the account.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
