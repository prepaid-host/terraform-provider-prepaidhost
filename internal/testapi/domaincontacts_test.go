// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

func contactBody() map[string]any {
	return map[string]any{
		"type": "PERS", "sex": "MALE", "firstname": "Max", "lastname": "Mustermann",
		"street": "Hauptstr.", "number": "1", "postcode": "12345", "city": "Berlin",
		"country": "de", "phone": "+49.3012345678", "email": "Max@Example.com",
		"idcard": "X1234567",
	}
}

func TestDomainContacts_CreateNormalizesAndHidesIdentification(t *testing.T) {
	api := testapi.New(t)
	resp, env := call(t, api, "POST", "/account/domain-contacts", api.Token, contactBody(), nil)
	if resp.StatusCode != 201 || !env.Success {
		t.Fatalf("create: %d %+v", resp.StatusCode, env)
	}
	var c map[string]any
	_ = json.Unmarshal(env.Result, &c)
	if c["email"] != "max@example.com" || c["country"] != "DE" || c["region"] != "Berlin" || c["is_default"] != true {
		t.Errorf("normalization: %v", c)
	}
	if _, leaked := c["idcard"]; leaked {
		t.Error("identification must never be echoed")
	}
	if len(c) != 18 {
		t.Errorf("fields = %d (%v), want the 18 of present()", len(c), c)
	}
}

func TestDomainContacts_Errors(t *testing.T) {
	api := testapi.New(t)

	bad := contactBody()
	delete(bad, "firstname")
	bad["sex"] = "X"
	resp, env := call(t, api, "POST", "/account/domain-contacts", api.Token, bad, nil)
	fields, _ := env.Error["fields"].(map[string]any)
	if resp.StatusCode != 422 || env.Error["code"] != "VALIDATION_FAILED" || fields["firstname"] == nil || fields["sex"] == nil {
		t.Errorf("validation: %d %+v", resp.StatusCode, env.Error)
	}

	org := contactBody()
	org["type"] = "ORG"
	resp, env = call(t, api, "POST", "/account/domain-contacts", api.Token, org, nil)
	if resp.StatusCode != 422 || env.Error["code"] != "ORG_REQUIRED" {
		t.Errorf("org: %d %+v", resp.StatusCode, env.Error)
	}

	_, env = call(t, api, "POST", "/account/domain-contacts", api.Token, contactBody(), nil)
	var created map[string]any
	_ = json.Unmarshal(env.Result, &created)
	resp, env = call(t, api, "POST", "/account/domain-contacts", api.Token, contactBody(), nil)
	if resp.StatusCode != 409 || env.Error["code"] != "DUPLICATE_CONTACT" || env.Error["existing_contact_id"] != created["id"] {
		t.Errorf("duplicate: %d %+v", resp.StatusCode, env.Error)
	}
}

func TestDomainContacts_MaxAndOrder(t *testing.T) {
	api := testapi.New(t)
	for i := 0; i < testapi.MaxDomainContacts; i++ {
		b := contactBody()
		b["firstname"] = fmt.Sprintf("Max%c", 'A'+i)
		if resp, env := call(t, api, "POST", "/account/domain-contacts", api.Token, b, nil); resp.StatusCode != 201 {
			t.Fatalf("create %d: %d %+v", i, resp.StatusCode, env.Error)
		}
	}
	b := contactBody()
	b["firstname"] = "Extra"
	resp, env := call(t, api, "POST", "/account/domain-contacts", api.Token, b, nil)
	if resp.StatusCode != 429 || env.Error["code"] != "MAX_CONTACTS" {
		t.Errorf("max: %d %+v", resp.StatusCode, env.Error)
	}

	_, env = call(t, api, "GET", "/account/domain-contacts", api.Token, nil, nil)
	var list []map[string]any
	_ = json.Unmarshal(env.Result, &list)
	if len(list) != testapi.MaxDomainContacts || list[0]["is_default"] != true || list[0]["firstname"] != "MaxA" {
		t.Errorf("default contact must come first: %v", list[0])
	}
	if list[1]["firstname"] != "MaxJ" {
		t.Errorf("then newest first: %v", list[1])
	}

	// No read-by-id and no delete in the real API.
	if resp, _ := call(t, api, "DELETE", "/account/domain-contacts/1", api.Token, nil, nil); resp.StatusCode != 404 {
		t.Errorf("DELETE status = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, api, "GET", "/account/domain-contacts/1", api.Token, nil, nil); resp.StatusCode != 404 {
		t.Errorf("GET by id status = %d, want 404", resp.StatusCode)
	}
}
