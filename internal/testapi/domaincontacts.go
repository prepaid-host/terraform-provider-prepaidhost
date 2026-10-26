// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// ResourceDomainContact is the State.Resources type of domain contacts.
const ResourceDomainContact = "domain_contact"

// MaxDomainContacts is how many domain contacts the API allows per account.
const MaxDomainContacts = 10

// GET and POST /account/domain-contacts like the API.
// The API has no read-by-id and no delete endpoint for contacts.
func init() {
	register(func(s *Server) {
		s.Handle(http.MethodGet, "/account/domain-contacts", handleListDomainContacts)
		s.Handle(http.MethodPost, "/account/domain-contacts", handleCreateDomainContact)
	})
}

// domainContactIdentification are the registrar identification fields:
// accepted on create, never echoed back.
var domainContactIdentification = map[string]string{
	"idcard": "string", "idcardissuedate": "date", "idcardauthority": "string",
	"taxnr": "string", "vatnr": "string", "dateofbirth": "date",
	"countryofbirth": "country", "placeofbirth": "string", "regionofbirth": "string",
	"registrationnumber": "string",
}

var (
	dateRE  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// presentDomainContact presents a contact like the API.
func presentDomainContact(attrs map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{
		"id", "handle_code", "api_handle_id", "name", "type", "sex", "firstname", "lastname",
		"organisation", "street", "number", "postcode", "city", "region", "country", "phone",
		"email", "is_default",
	} {
		out[k] = attrs[k]
	}
	return out
}

func handleListDomainContacts(st *State, _ *Request) Response {
	ids := st.ResourceIDs(ResourceDomainContact)
	// orderByDesc('is_default')->orderByDesc('id')
	sort.SliceStable(ids, func(i, j int) bool {
		a, _ := st.Resource(ResourceDomainContact, ids[i])
		b, _ := st.Resource(ResourceDomainContact, ids[j])
		ad, _ := a["is_default"].(bool)
		bd, _ := b["is_default"].(bool)
		if ad != bd {
			return ad
		}
		return ids[i] > ids[j]
	})
	list := []any{}
	for _, id := range ids {
		attrs, _ := st.Resource(ResourceDomainContact, id)
		list = append(list, presentDomainContact(attrs))
	}
	return OK(list)
}

func str(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return strings.TrimSpace(s)
}

// validateDomainContact applies the rules of StoreDomainHandleRequest (the
// subset that matters for the provider) and returns the error bag.
func validateDomainContact(body map[string]any) map[string]any {
	bag := map[string]any{}
	add := func(field, msg string) { bag[field] = []any{msg} }

	required := []string{"type", "sex", "firstname", "lastname", "street", "number", "postcode", "city", "country", "phone", "email"}
	for _, f := range required {
		if str(body, f) == "" {
			add(f, fmt.Sprintf("The %s field is required.", f))
		}
	}
	if v := str(body, "type"); v != "" && v != "PERS" && v != "ORG" {
		add("type", "The selected type is invalid.")
	}
	if v := str(body, "sex"); v != "" && v != "MALE" && v != "FEMALE" && v != "UNKNOWN" {
		add("sex", "The selected sex is invalid.")
	}
	if v := str(body, "country"); v != "" && len(v) != 2 {
		add("country", "The country field must be 2 characters.")
	}
	if v := str(body, "email"); v != "" && !emailRE.MatchString(v) {
		add("email", "The email field must be a valid email address.")
	}
	for f, kind := range domainContactIdentification {
		v, ok := body[f].(string)
		if !ok || v == "" {
			continue
		}
		if kind == "date" && !dateRE.MatchString(v) {
			add(f, fmt.Sprintf("The %s field must match the format Y-m-d.", f))
		}
		if kind == "country" && len(v) != 2 {
			add(f, fmt.Sprintf("The %s field must be 2 characters.", f))
		}
	}
	return bag
}

func handleCreateDomainContact(st *State, r *Request) Response {
	body := r.BodyMap()
	if bag := validateDomainContact(body); len(bag) > 0 {
		return Fail(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation failed.", map[string]any{"fields": bag})
	}

	// Create, with the per-account limit.
	ids := st.ResourceIDs(ResourceDomainContact)
	if len(ids) >= MaxDomainContacts {
		return Fail(http.StatusTooManyRequests, "MAX_CONTACTS", "The maximum number of contacts is reached.", nil)
	}

	clean := map[string]any{
		"name":         str(body, "name"),
		"type":         str(body, "type"),
		"sex":          str(body, "sex"),
		"firstname":    str(body, "firstname"),
		"lastname":     str(body, "lastname"),
		"organisation": str(body, "organisation"),
		"street":       str(body, "street"),
		"number":       str(body, "number"),
		"postcode":     str(body, "postcode"),
		"city":         str(body, "city"),
		"region":       str(body, "region"),
		"country":      strings.ToUpper(str(body, "country")),
		"phone":        str(body, "phone"),
		"email":        strings.ToLower(str(body, "email")),
	}
	if clean["region"] == "" {
		clean["region"] = clean["city"]
	}
	if clean["type"] == "ORG" && clean["organisation"] == "" {
		return Fail(http.StatusUnprocessableEntity, "ORG_REQUIRED", "An organisation name is required for organisations.", nil)
	}
	for _, id := range ids {
		existing, _ := st.Resource(ResourceDomainContact, id)
		if existing["firstname"] == clean["firstname"] && existing["lastname"] == clean["lastname"] && existing["email"] == clean["email"] {
			return Fail(http.StatusConflict, "DUPLICATE_CONTACT", "A contact with this name and email already exists.",
				map[string]any{"existing_contact_id": id})
		}
	}

	isDefault, _ := body["is_default"].(bool)
	clean["is_default"] = false
	clean["api_handle_id"] = nil
	clean["handle_code"] = nil
	for f := range domainContactIdentification {
		if v := str(body, f); v != "" {
			clean[f] = v
		}
	}
	id := st.AddResource(ResourceDomainContact, clean)
	clean["handle_code"] = fmt.Sprintf("HDL-%d", id)
	clean["api_handle_id"] = fmt.Sprintf("NIC-%d", id)
	if isDefault || len(ids) == 0 {
		for _, other := range ids {
			o, _ := st.Resource(ResourceDomainContact, other)
			o["is_default"] = false
		}
		clean["is_default"] = true
	}
	st.PutResource(ResourceDomainContact, id, clean)
	attrs, _ := st.Resource(ResourceDomainContact, id)
	return Created(presentDomainContact(attrs))
}

// SeedDomainContact stores a contact as if it had been created outside
// Terraform. attrs use the API's field names (overrides of a valid default).
func (s *Server) SeedDomainContact(attrs map[string]any) int64 {
	c := map[string]any{
		"name": "", "type": "PERS", "sex": "MALE", "firstname": "Max", "lastname": "Mustermann",
		"organisation": "", "street": "Hauptstr.", "number": "1", "postcode": "12345", "city": "Berlin",
		"region": "Berlin", "country": "DE", "phone": "+49.3012345678", "email": "max@example.com",
		"is_default": false, "api_handle_id": nil, "handle_code": nil,
	}
	for k, v := range attrs {
		c[k] = v
	}
	return s.SeedResource(ResourceDomainContact, c)
}
