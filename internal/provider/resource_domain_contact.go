// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

var (
	_ resource.Resource                = &domainContactResource{}
	_ resource.ResourceWithConfigure   = &domainContactResource{}
	_ resource.ResourceWithImportState = &domainContactResource{}
	_ resource.ResourceWithModifyPlan  = &domainContactResource{}
)

var dateFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// NewDomainContactResource returns the prepaidhost_domain_contact resource.
func NewDomainContactResource() resource.Resource { return &domainContactResource{} }

type domainContactResource struct {
	client *client.Client
}

type domainContactModel struct {
	ID          types.String `tfsdk:"id"`
	HandleCode  types.String `tfsdk:"handle_code"`
	APIHandleID types.String `tfsdk:"api_handle_id"`

	Name         types.String `tfsdk:"name"`
	Type         types.String `tfsdk:"type"`
	Sex          types.String `tfsdk:"sex"`
	Firstname    types.String `tfsdk:"firstname"`
	Lastname     types.String `tfsdk:"lastname"`
	Organisation types.String `tfsdk:"organisation"`
	Street       types.String `tfsdk:"street"`
	Number       types.String `tfsdk:"number"`
	Postcode     types.String `tfsdk:"postcode"`
	City         types.String `tfsdk:"city"`
	Region       types.String `tfsdk:"region"`
	Country      types.String `tfsdk:"country"`
	Phone        types.String `tfsdk:"phone"`
	Fax          types.String `tfsdk:"fax"`
	Email        types.String `tfsdk:"email"`
	IsDefault    types.Bool   `tfsdk:"is_default"`

	IDCard             types.String `tfsdk:"idcard"`
	IDCardIssueDate    types.String `tfsdk:"idcardissuedate"`
	IDCardAuthority    types.String `tfsdk:"idcardauthority"`
	TaxNr              types.String `tfsdk:"taxnr"`
	VatNr              types.String `tfsdk:"vatnr"`
	DateOfBirth        types.String `tfsdk:"dateofbirth"`
	CountryOfBirth     types.String `tfsdk:"countryofbirth"`
	PlaceOfBirth       types.String `tfsdk:"placeofbirth"`
	RegionOfBirth      types.String `tfsdk:"regionofbirth"`
	RegistrationNumber types.String `tfsdk:"registrationnumber"`
}

// domainContactAPI is one entry of GET /account/domain-contacts
// (the API presents no fax and no identification).
type domainContactAPI struct {
	ID           int64   `json:"id"`
	HandleCode   *string `json:"handle_code"`
	APIHandleID  *string `json:"api_handle_id"`
	Name         *string `json:"name"`
	Type         string  `json:"type"`
	Sex          *string `json:"sex"`
	Firstname    *string `json:"firstname"`
	Lastname     *string `json:"lastname"`
	Organisation *string `json:"organisation"`
	Street       string  `json:"street"`
	Number       string  `json:"number"`
	Postcode     string  `json:"postcode"`
	City         string  `json:"city"`
	Region       *string `json:"region"`
	Country      string  `json:"country"`
	Phone        string  `json:"phone"`
	Email        string  `json:"email"`
	IsDefault    bool    `json:"is_default"`
}

// identificationFields returns the registrar identification fields by API name.
func (m *domainContactModel) identificationFields() map[string]types.String {
	return map[string]types.String{
		"idcard":             m.IDCard,
		"idcardissuedate":    m.IDCardIssueDate,
		"idcardauthority":    m.IDCardAuthority,
		"taxnr":              m.TaxNr,
		"vatnr":              m.VatNr,
		"dateofbirth":        m.DateOfBirth,
		"countryofbirth":     m.CountryOfBirth,
		"placeofbirth":       m.PlaceOfBirth,
		"regionofbirth":      m.RegionOfBirth,
		"registrationnumber": m.RegistrationNumber,
	}
}

// contactText describes a string attribute.
type contactText struct {
	desc       string
	required   bool
	computed   bool // optional and filled by the API when not configured
	sensitive  bool
	writeOnly  bool // not returned by the API
	validators []validator.String
}

func replaceString(t contactText) schema.StringAttribute {
	mods := []planmodifier.String{}
	if t.computed {
		mods = append(mods, stringplanmodifier.UseStateForUnknown())
	}
	if t.writeOnly {
		// The API never returns these: a null state value means "imported, unknown"
		// and the configured value is adopted in place (see Update).
		desc := "Replaces the contact when the value changes, unless it was unknown after an import."
		mods = append(mods, stringplanmodifier.RequiresReplaceIf(
			func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
				resp.RequiresReplace = !req.StateValue.IsNull()
			}, desc, desc))
	} else {
		mods = append(mods, stringplanmodifier.RequiresReplace())
	}
	return schema.StringAttribute{
		MarkdownDescription: t.desc,
		Required:            t.required,
		Optional:            !t.required,
		Computed:            t.computed,
		Sensitive:           t.sensitive,
		PlanModifiers:       mods,
		Validators:          t.validators,
	}
}

func (r *domainContactResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain_contact"
}

func (r *domainContactResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	date := []validator.String{stringvalidator.RegexMatches(dateFormat, "must be a date in the format YYYY-MM-DD")}
	country2 := []validator.String{stringvalidator.LengthBetween(2, 2)}
	ident := func(desc string, v ...validator.String) schema.StringAttribute {
		return replaceString(contactText{desc: desc + " Only needed for TLDs that require identification of the holder (for example `.it`). The API never returns it.", sensitive: true, writeOnly: true, validators: v})
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A registrant contact for domain orders (`POST /account/domain-contacts`). " +
			"Needs a token with the `domain:contact` scope; the contact `id` is used as the holder when ordering a domain.\n\n" +
			"**The API can neither change nor delete contacts.** Every argument change therefore replaces the resource, which only works " +
			"when first name, last name and email differ (the API refuses a second contact with the same ones). " +
			"Destroying the resource only removes it from the Terraform state; the contact stays in the account and can be removed in the customer panel. " +
			"If an identical contact already exists, it is adopted instead of created. " +
			"`fax` and the identification fields are never returned by the API: after `terraform import` they are null in the state, " +
			"and the first apply with a configured value adopts it in place without replacing the contact.\n\n" +
			"An account holds at most 10 contacts.\n\n" +
			"~> **Note:** Sensitive values (the identification fields) are hidden in plan output but are still stored in the state file in plain text. Protect your state.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Contact id (the holder id for domain orders).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"handle_code": schema.StringAttribute{
				MarkdownDescription: "Handle code of the contact; null until the contact is used.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"api_handle_id": schema.StringAttribute{
				MarkdownDescription: "Handle id at the registrar; null if the registrar was not reachable on creation.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name":         replaceString(contactText{desc: "Optional label of the contact."}),
			"type":         replaceString(contactText{desc: "`PERS` (person) or `ORG` (organisation; needs `organisation`).", required: true, validators: []validator.String{stringvalidator.OneOf("PERS", "ORG")}}),
			"sex":          replaceString(contactText{desc: "`MALE`, `FEMALE` or `UNKNOWN`.", required: true, validators: []validator.String{stringvalidator.OneOf("MALE", "FEMALE", "UNKNOWN")}}),
			"firstname":    replaceString(contactText{desc: "First name.", required: true}),
			"lastname":     replaceString(contactText{desc: "Last name.", required: true}),
			"organisation": replaceString(contactText{desc: "Organisation name. Required for `type = \"ORG\"`."}),
			"street":       replaceString(contactText{desc: "Street without house number.", required: true}),
			"number":       replaceString(contactText{desc: "House number.", required: true}),
			"postcode":     replaceString(contactText{desc: "Postal code.", required: true}),
			"city":         replaceString(contactText{desc: "City.", required: true}),
			"region":       replaceString(contactText{desc: "Region or state. Defaults to the city.", computed: true}),
			"country":      replaceString(contactText{desc: "Two-letter ISO 3166-1 country code, for example `DE`.", required: true, validators: country2}),
			"phone":        replaceString(contactText{desc: "Phone number in international format, for example `+49.3012345678`.", required: true}),
			"fax":          replaceString(contactText{desc: "Fax number. The API does not return it.", writeOnly: true}),
			"email":        replaceString(contactText{desc: "Email address. The API stores it in lowercase.", required: true}),
			"is_default": schema.BoolAttribute{
				MarkdownDescription: "Make this the default contact. Evaluated when the contact is created; the first contact of an account is always the default. " +
					"Read from the API on import only.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown(), boolplanmodifier.RequiresReplace()},
			},

			"idcard":             ident("Identity card number.", stringvalidator.LengthAtLeast(1)),
			"idcardissuedate":    ident("Issue date of the identity card (`YYYY-MM-DD`).", date...),
			"idcardauthority":    ident("Authority that issued the identity card."),
			"taxnr":              ident("Tax number."),
			"vatnr":              ident("VAT id."),
			"dateofbirth":        ident("Date of birth (`YYYY-MM-DD`).", date...),
			"countryofbirth":     ident("Country of birth, two-letter ISO code.", country2...),
			"placeofbirth":       ident("Place of birth."),
			"regionofbirth":      ident("Region of birth."),
			"registrationnumber": ident("Commercial register number of an organisation."),
		},
	}
}

func (r *domainContactResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// equivalent reports whether the API value is what was configured, allowing
// for the normalization the API applies (trim, lowercase email, uppercase country).
func equivalent(configured, api string) bool {
	return strings.EqualFold(strings.TrimSpace(configured), strings.TrimSpace(api))
}

// syncString merges a value the API returned into the value in state/plan:
// the configured spelling is kept when the API only normalized it, an empty
// API value means null.
func syncString(current types.String, api *string) types.String {
	if api == nil || *api == "" {
		return types.StringNull()
	}
	if !current.IsNull() && !current.IsUnknown() && equivalent(current.ValueString(), *api) {
		return current
	}
	return types.StringValue(*api)
}

// fromAPI merges the API's view of the contact into m. Values that are only
// written (fax, identification) and is_default are kept unless m has none yet
// (import).
func (m *domainContactModel) fromAPI(c domainContactAPI) {
	m.ID = types.StringValue(strconv.FormatInt(c.ID, 10))
	m.HandleCode = optionalString(c.HandleCode)
	m.APIHandleID = optionalString(c.APIHandleID)

	m.Name = syncString(m.Name, c.Name)
	m.Type = syncString(m.Type, &c.Type)
	m.Sex = syncString(m.Sex, c.Sex)
	m.Firstname = syncString(m.Firstname, c.Firstname)
	m.Lastname = syncString(m.Lastname, c.Lastname)
	m.Organisation = syncString(m.Organisation, c.Organisation)
	m.Street = syncString(m.Street, &c.Street)
	m.Number = syncString(m.Number, &c.Number)
	m.Postcode = syncString(m.Postcode, &c.Postcode)
	m.City = syncString(m.City, &c.City)
	m.Region = syncString(m.Region, c.Region)
	m.Country = syncString(m.Country, &c.Country)
	m.Phone = syncString(m.Phone, &c.Phone)
	m.Email = syncString(m.Email, &c.Email)
	if m.IsDefault.IsNull() || m.IsDefault.IsUnknown() {
		m.IsDefault = types.BoolValue(c.IsDefault)
	}
}

func optionalString(s *string) types.String {
	if s == nil {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

// requestBody is the POST body: every configured attribute under its API name.
func (m *domainContactModel) requestBody() map[string]any {
	body := map[string]any{}
	set := func(key string, v types.String) {
		if !v.IsNull() && !v.IsUnknown() {
			body[key] = v.ValueString()
		}
	}
	set("name", m.Name)
	set("type", m.Type)
	set("sex", m.Sex)
	set("firstname", m.Firstname)
	set("lastname", m.Lastname)
	set("organisation", m.Organisation)
	set("street", m.Street)
	set("number", m.Number)
	set("postcode", m.Postcode)
	set("city", m.City)
	set("region", m.Region)
	set("country", m.Country)
	set("phone", m.Phone)
	set("fax", m.Fax)
	set("email", m.Email)
	if !m.IsDefault.IsNull() && !m.IsDefault.IsUnknown() {
		body["is_default"] = m.IsDefault.ValueBool()
	}
	for key, v := range m.identificationFields() {
		set(key, v)
	}
	return body
}

// differences lists the configured attributes that do not match the existing contact.
func (m *domainContactModel) differences(c domainContactAPI) []string {
	var diffs []string
	check := func(name string, configured types.String, api string) {
		if configured.IsNull() || configured.IsUnknown() {
			return
		}
		if !equivalent(configured.ValueString(), api) {
			diffs = append(diffs, name)
		}
	}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	check("name", m.Name, deref(c.Name))
	check("type", m.Type, c.Type)
	check("sex", m.Sex, deref(c.Sex))
	check("firstname", m.Firstname, deref(c.Firstname))
	check("lastname", m.Lastname, deref(c.Lastname))
	check("organisation", m.Organisation, deref(c.Organisation))
	check("street", m.Street, c.Street)
	check("number", m.Number, c.Number)
	check("postcode", m.Postcode, c.Postcode)
	check("city", m.City, c.City)
	check("region", m.Region, deref(c.Region))
	check("country", m.Country, c.Country)
	check("phone", m.Phone, c.Phone)
	check("email", m.Email, c.Email)
	if !m.IsDefault.IsNull() && !m.IsDefault.IsUnknown() && m.IsDefault.ValueBool() != c.IsDefault {
		diffs = append(diffs, "is_default")
	}
	return diffs
}

func (r *domainContactResource) list(ctx context.Context) ([]domainContactAPI, error) {
	var contacts []domainContactAPI
	err := r.client.Do(ctx, http.MethodGet, "/account/domain-contacts", nil, &contacts, nil)
	return contacts, err
}

func findContact(contacts []domainContactAPI, id string) *domainContactAPI {
	for i := range contacts {
		if strconv.FormatInt(contacts[i].ID, 10) == id {
			return &contacts[i]
		}
	}
	return nil
}

// fillUnknown turns the computed attributes the plan could not know into null
// or the API's value, so that no unknown is left in the state.
func (m *domainContactModel) settle() {
	for _, s := range []*types.String{&m.HandleCode, &m.APIHandleID, &m.Region} {
		if s.IsUnknown() {
			*s = types.StringNull()
		}
	}
	if m.IsDefault.IsUnknown() {
		m.IsDefault = types.BoolNull()
	}
}

func (r *domainContactResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainContactModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var created domainContactAPI
	err := r.client.Do(ctx, http.MethodPost, "/account/domain-contacts", plan.requestBody(), &created, nil)
	if err != nil {
		if client.IsCode(err, client.CodeDuplicateContact) {
			r.adoptDuplicate(ctx, &plan, err, resp)
			return
		}
		addAPIError(&resp.Diagnostics, "Could not create the domain contact", err)
		return
	}

	plan.fromAPI(created)
	plan.settle()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// adoptDuplicate handles DUPLICATE_CONTACT: the API refuses a contact with the
// same first name, last name and email as an existing one and names it. If the
// existing contact matches the configuration it becomes this resource; if not,
// Terraform cannot make it match (no update in the API), which is an error.
func (r *domainContactResource) adoptDuplicate(ctx context.Context, plan *domainContactModel, dupErr error, resp *resource.CreateResponse) {
	apiErr, _ := client.AsAPIError(dupErr)
	existingID, ok := apiErr.Int64("existing_contact_id")
	if !ok {
		addAPIError(&resp.Diagnostics, "Could not create the domain contact", dupErr)
		return
	}
	id := strconv.FormatInt(existingID, 10)

	contacts, err := r.list(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the domain contacts to adopt the existing one", err)
		return
	}
	existing := findContact(contacts, id)
	if existing == nil {
		addAPIError(&resp.Diagnostics, "Could not create the domain contact", dupErr)
		return
	}

	if diffs := plan.differences(*existing); len(diffs) > 0 {
		resp.Diagnostics.AddError("Domain contact exists with different values",
			fmt.Sprintf("%s\n\nThe contact %s already exists and differs in: %s. "+
				"The API can neither change nor delete contacts, so Terraform cannot make it match. "+
				"Either adjust the configuration to the existing contact and import it (`terraform import <address> %s`), "+
				"or use a different first name, last name or email for a new contact.",
				apiErr.Error(), id, strings.Join(diffs, ", "), id))
		return
	}

	plan.fromAPI(*existing)
	plan.settle()
	resp.Diagnostics.AddWarning("Existing domain contact adopted",
		fmt.Sprintf("A contact with this name and email already exists (id %s) and matches the configuration, so it is managed by this resource now. "+
			"Identification fields cannot be read back from the API and were not compared.", id))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *domainContactResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainContactModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The API has no read-by-id: the contact must be in the list.
	contacts, err := r.list(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the domain contacts", err)
		return
	}
	c := findContact(contacts, state.ID.ValueString())
	if c == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.fromAPI(*c)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only runs to adopt fax/identification values after an import (every
// other change replaces the contact); nothing is sent to the API.
func (r *domainContactResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state domainContactModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	plan.HandleCode = state.HandleCode
	plan.APIHandleID = state.APIHandleID
	plan.settle()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// userAttributes are the arguments the API takes on create, by name.
func (m *domainContactModel) userAttributes() map[string]types.String {
	attrs := map[string]types.String{
		"name": m.Name, "type": m.Type, "sex": m.Sex, "firstname": m.Firstname, "lastname": m.Lastname,
		"organisation": m.Organisation, "street": m.Street, "number": m.Number, "postcode": m.Postcode,
		"city": m.City, "region": m.Region, "country": m.Country, "phone": m.Phone, "fax": m.Fax, "email": m.Email,
	}
	for k, v := range m.identificationFields() {
		attrs[k] = v
	}
	return attrs
}

var writeOnlyContactAttributes = map[string]bool{
	"fax": true, "idcard": true, "idcardissuedate": true, "idcardauthority": true, "taxnr": true,
	"vatnr": true, "dateofbirth": true, "countryofbirth": true, "placeofbirth": true,
	"regionofbirth": true, "registrationnumber": true,
}

// ModifyPlan refuses a replacement that cannot succeed: the API treats a
// contact with the same first name, last name and email as a duplicate, and
// cannot delete the old one, so the create after the replace would fail on
// every apply.
func (r *domainContactResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state domainContactModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	stateAttrs := state.userAttributes()
	var changed []string
	for name, p := range plan.userAttributes() {
		if p.IsUnknown() {
			continue
		}
		s := stateAttrs[name]
		if writeOnlyContactAttributes[name] && s.IsNull() {
			continue // adopted in place
		}
		loose := name == "firstname" || name == "lastname" || name == "email"
		if (loose && !equivalent(p.ValueString(), s.ValueString())) || (!loose && !p.Equal(s)) {
			changed = append(changed, name)
		}
	}
	if !plan.IsDefault.IsUnknown() && !plan.IsDefault.Equal(state.IsDefault) {
		changed = append(changed, "is_default")
	}
	if len(changed) == 0 {
		return
	}
	for _, k := range []string{"firstname", "lastname", "email"} {
		if !equivalent(plan.userAttributes()[k].ValueString(), stateAttrs[k].ValueString()) {
			return // a new contact can be created
		}
	}
	sort.Strings(changed)
	resp.Diagnostics.AddError("Domain contact cannot be changed",
		fmt.Sprintf("Changing %s would replace contact %s, but the API can neither change nor delete contacts and refuses a new contact "+
			"with the same first name, last name and email (DUPLICATE_CONTACT). To create a new contact change the first name, last name or email; "+
			"to use the existing one keep the configuration as it is, or import the matching contact (`terraform import <address> <id>`) and change the "+
			"configuration to it.\n\nIf the contact was changed in the customer panel, set the configuration to the panel values or revert the change "+
			"there; alternatively `terraform state rm <address>` and import the matching contact.", strings.Join(changed, ", "), state.ID.ValueString()))
}

// Delete only forgets the contact: the API has no delete endpoint.
func (r *domainContactResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning("Domain contact remains in the account",
		"The API cannot delete domain contacts. The contact was removed from the Terraform state only and remains in the account; "+
			"remove it in the customer panel if it is no longer needed.")
}

func (r *domainContactResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := strconv.ParseInt(req.ID, 10, 64); err != nil {
		resp.Diagnostics.AddError("Invalid domain contact id", fmt.Sprintf("The import id must be the numeric contact id, got %q.", req.ID))
		return
	}
	contacts, err := r.list(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the domain contacts", err)
		return
	}
	if findContact(contacts, req.ID) == nil {
		resp.Diagnostics.AddError("Domain contact not found", fmt.Sprintf("No domain contact with id %s was found in the account.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
