// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

const contactAddr = "prepaidhost_domain_contact.owner"

var identificationFields = []string{
	"idcard", "idcardissuedate", "idcardauthority", "taxnr", "vatnr", "dateofbirth",
	"countryofbirth", "placeofbirth", "regionofbirth", "registrationnumber",
}

// contactConfig is a complete, valid contact; overrides replace or (value "")
// drop an attribute. Values are written as HCL strings, except booleans.
func contactConfig(api *testapi.Server, overrides map[string]string) string {
	attrs := map[string]string{
		"type": "PERS", "sex": "MALE", "firstname": "Max", "lastname": "Mustermann",
		"street": "Hauptstr.", "number": "1", "postcode": "12345", "city": "Berlin",
		"country": "DE", "phone": "+49.3012345678", "email": "max@example.com",
	}
	for k, v := range overrides {
		attrs[k] = v
	}
	order := []string{
		"type", "sex", "firstname", "lastname", "organisation", "name", "street", "number", "postcode",
		"city", "region", "country", "phone", "fax", "email", "is_default",
	}
	order = append(order, identificationFields...)

	var b strings.Builder
	b.WriteString(providerConfig(api))
	b.WriteString("\nresource \"prepaidhost_domain_contact\" \"owner\" {\n")
	for _, k := range order {
		v, ok := attrs[k]
		if !ok || v == "" {
			continue
		}
		if k == "is_default" {
			fmt.Fprintf(&b, "  %s = %s\n", k, v)
		} else {
			fmt.Fprintf(&b, "  %s = %q\n", k, v)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

func contactCount(api *testapi.Server) int {
	return len(api.ResourceIDs(testapi.ResourceDomainContact))
}

func TestAccDomainContact_CreateReadImport(t *testing.T) {
	api := testapi.New(t)

	accTest(t,
		tfresource.TestStep{
			Config: contactConfig(api, map[string]string{
				"organisation": "Muster GmbH", "fax": "+49.3012345679", "is_default": "true",
				"idcard": "X1234567", "dateofbirth": "1990-02-03",
			}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrSet(contactAddr, "id"),
				tfresource.TestCheckResourceAttrSet(contactAddr, "handle_code"),
				tfresource.TestCheckResourceAttrSet(contactAddr, "api_handle_id"),
				tfresource.TestCheckResourceAttr(contactAddr, "type", "PERS"),
				tfresource.TestCheckResourceAttr(contactAddr, "firstname", "Max"),
				tfresource.TestCheckResourceAttr(contactAddr, "organisation", "Muster GmbH"),
				// region was not configured: the API defaults it to the city.
				tfresource.TestCheckResourceAttr(contactAddr, "region", "Berlin"),
				tfresource.TestCheckResourceAttr(contactAddr, "is_default", "true"),
				tfresource.TestCheckResourceAttr(contactAddr, "idcard", "X1234567"),
				tfresource.TestCheckNoResourceAttr(contactAddr, "taxnr"),
				func(*terraform.State) error {
					ids := api.ResourceIDs(testapi.ResourceDomainContact)
					if len(ids) != 1 {
						return fmt.Errorf("mock has %d contacts, want 1", len(ids))
					}
					c, _ := api.Resource(testapi.ResourceDomainContact, ids[0])
					if c["idcard"] != "X1234567" || c["dateofbirth"] != "1990-02-03" {
						return fmt.Errorf("identification was not sent to the API: %v", c)
					}
					return nil
				},
			),
		},
		// The API returns neither fax nor the identification fields, and is_default
		// is only taken over on import.
		tfresource.TestStep{
			ResourceName:      contactAddr,
			ImportState:       true,
			ImportStateVerify: true,
			ImportStateVerifyIgnore: append([]string{"fax"},
				identificationFields...),
		},
	)
}

func TestAccDomainContact_NormalizedValuesCauseNoDiff(t *testing.T) {
	api := testapi.New(t)
	// The API lowercases the email, uppercases the country and trims text.
	// accTest fails when the plan after apply or refresh is not empty.
	accTest(t,
		tfresource.TestStep{
			Config: contactConfig(api, map[string]string{"email": "Max@Example.COM", "country": "de"}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr(contactAddr, "email", "Max@Example.COM"),
				tfresource.TestCheckResourceAttr(contactAddr, "country", "de"),
			),
		},
		tfresource.TestStep{
			RefreshState: true,
			PlanOnly:     false,
		},
	)
}

func TestAccDomainContact_DestroyKeepsContactInAccount(t *testing.T) {
	api := testapi.New(t)
	accTest(t, tfresource.TestStep{Config: contactConfig(api, nil)})

	// terraform destroy ran: the contact is still there, and nothing was sent besides the POST.
	if n := contactCount(api); n != 1 {
		t.Errorf("contacts after destroy = %d, want 1 (no delete endpoint)", n)
	}
	if got := api.WriteCount(); got != 1 {
		t.Errorf("write calls = %d, want only the create POST", got)
	}
}

func TestDomainContact_DeleteWarns(t *testing.T) {
	var resp resource.DeleteResponse
	NewDomainContactResource().Delete(context.Background(), resource.DeleteRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete must not fail: %v", resp.Diagnostics)
	}
	warnings := resp.Diagnostics.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0].Detail(), "remains") {
		t.Errorf("want one warning that the contact remains in the account, got %v", resp.Diagnostics)
	}
}

func TestDomainContact_IdentificationIsSensitive(t *testing.T) {
	var resp resource.SchemaResponse
	NewDomainContactResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)

	for _, f := range identificationFields {
		a, ok := resp.Schema.Attributes[f]
		if !ok || !a.IsSensitive() || !a.IsOptional() {
			t.Errorf("%s must be optional and sensitive", f)
		}
	}
	for _, f := range []string{"firstname", "email", "street"} {
		if resp.Schema.Attributes[f].IsSensitive() {
			t.Errorf("%s should not be sensitive", f)
		}
	}
}

func TestAccDomainContact_ReplacedWhenChanged(t *testing.T) {
	api := testapi.New(t)
	var first string

	accTest(t,
		tfresource.TestStep{
			Config: contactConfig(api, nil),
			Check: tfresource.TestCheckResourceAttrWith(contactAddr, "id", func(v string) error {
				first = v
				return nil
			}),
		},
		// Name and email are the API's duplicate key: a changed email is a new contact.
		tfresource.TestStep{
			Config: contactConfig(api, map[string]string{"email": "max.neu@example.com"}),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttrWith(contactAddr, "id", func(v string) error {
					if v == first {
						return fmt.Errorf("contact was not replaced")
					}
					return nil
				}),
				tfresource.TestCheckResourceAttr(contactAddr, "email", "max.neu@example.com"),
			),
		},
	)
}

func TestAccDomainContact_ChangeWithSameNameAndEmailCannotBeApplied(t *testing.T) {
	api := testapi.New(t)

	accTest(t,
		tfresource.TestStep{Config: contactConfig(api, nil)},
		// A new street is a replacement, which the API answers with DUPLICATE_CONTACT
		// (same name and email) for a contact that differs: no silent adoption.
		tfresource.TestStep{
			Config:      contactConfig(api, map[string]string{"street": "Nebenstr."}),
			ExpectError: re(`(?s)DUPLICATE_CONTACT.*street`),
		},
	)
}

func TestAccDomainContact_DuplicateIsAdopted(t *testing.T) {
	api := testapi.New(t)
	existing := api.SeedDomainContact(map[string]any{"email": "max@example.com"})

	accTest(t, tfresource.TestStep{
		// Differences in case/whitespace only: the same contact.
		Config: contactConfig(api, map[string]string{"email": "Max@Example.com", "country": "de"}),
		Check: tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(contactAddr, "id", strconv.FormatInt(existing, 10)),
			func(*terraform.State) error {
				if n := contactCount(api); n != 1 {
					return fmt.Errorf("adoption created a second contact (%d)", n)
				}
				return nil
			},
		),
	})
	if got := api.CallCount("POST", "/account/domain-contacts"); got != 1 {
		t.Errorf("POST calls = %d, want 1 (the one answered with DUPLICATE_CONTACT)", got)
	}
}

func TestAccDomainContact_DuplicateWithDifferentValuesIsAnError(t *testing.T) {
	api := testapi.New(t)
	existing := api.SeedDomainContact(map[string]any{"street": "Andere Str.", "city": "Hamburg", "region": "Hamburg"})

	accTest(t, tfresource.TestStep{
		Config:      contactConfig(api, nil),
		ExpectError: re(fmt.Sprintf(`(?s)DUPLICATE_CONTACT.*%d.*(street|city)`, existing)),
	})
	if n := contactCount(api); n != 1 {
		t.Errorf("contacts = %d, want only the seeded one", n)
	}
}

func TestAccDomainContact_APIErrors(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		api := testapi.New(t)
		accTest(t, tfresource.TestStep{
			Config:      contactConfig(api, map[string]string{"email": "not-an-email"}),
			ExpectError: re(`(?s)VALIDATION_FAILED.*email`),
		})
	})
	t.Run("org required", func(t *testing.T) {
		api := testapi.New(t)
		accTest(t, tfresource.TestStep{
			Config:      contactConfig(api, map[string]string{"type": "ORG"}),
			ExpectError: re(`ORG_REQUIRED`),
		})
	})
	t.Run("max contacts", func(t *testing.T) {
		api := testapi.New(t)
		for i := 0; i < testapi.MaxDomainContacts; i++ {
			api.SeedDomainContact(map[string]any{"firstname": fmt.Sprintf("Other%c", 'A'+i)})
		}
		accTest(t, tfresource.TestStep{
			Config:      contactConfig(api, nil),
			ExpectError: re(`MAX_CONTACTS`),
		})
	})
}

func TestAccDomainContact_PlanTimeValidation(t *testing.T) {
	api := testapi.New(t)
	for name, overrides := range map[string]map[string]string{
		"type":            {"type": "PERSON"},
		"sex":             {"sex": "OTHER"},
		"country":         {"country": "DEU"},
		"dateofbirth":     {"dateofbirth": "03.02.1990"},
		"countryofbirth":  {"countryofbirth": "Germany"},
		"idcardissuedate": {"idcardissuedate": "2020/01/01"},
	} {
		accTest(t, tfresource.TestStep{
			Config:      contactConfig(api, overrides),
			PlanOnly:    true,
			ExpectError: re(name),
		})
	}
	if api.CallCount("POST", "/account/domain-contacts") != 0 {
		t.Error("invalid values must be rejected before any API call")
	}
}

func TestAccDomainContact_RemovedOutsideTerraformIsRecreated(t *testing.T) {
	api := testapi.New(t)
	var id string

	accTest(t,
		tfresource.TestStep{
			Config: contactConfig(api, nil),
			Check: tfresource.TestCheckResourceAttrWith(contactAddr, "id", func(v string) error {
				id = v
				return nil
			}),
		},
		tfresource.TestStep{
			PreConfig: func() {
				n, _ := strconv.ParseInt(id, 10, 64)
				api.DeleteResource(testapi.ResourceDomainContact, n)
			},
			RefreshState:       true,
			ExpectNonEmptyPlan: true,
		},
		tfresource.TestStep{
			Config: contactConfig(api, nil),
			Check:  tfresource.TestCheckResourceAttrSet(contactAddr, "id"),
		},
	)
}

func TestAccDomainContact_ChangedInPanelShowsDrift(t *testing.T) {
	api := testapi.New(t)
	var id string

	accTest(t,
		tfresource.TestStep{
			Config: contactConfig(api, nil),
			Check: tfresource.TestCheckResourceAttrWith(contactAddr, "id", func(v string) error {
				id = v
				return nil
			}),
		},
		tfresource.TestStep{
			PreConfig: func() {
				n, _ := strconv.ParseInt(id, 10, 64)
				api.UpdateResource(testapi.ResourceDomainContact, n, func(a map[string]any) { a["street"] = "Geaendert" })
			},
			Config:             contactConfig(api, nil),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
	)
}

func TestAccDomainContact_ImportUnknownID(t *testing.T) {
	api := testapi.New(t)
	accTest(t,
		tfresource.TestStep{
			Config:        importConfig(contactConfig(api, nil)),
			ResourceName:  contactAddr,
			ImportState:   true,
			ImportStateId: "4711",
			ExpectError:   re(`(?s)(contact not found|No domain contact).*4711`),
		},
		tfresource.TestStep{
			Config:        importConfig(contactConfig(api, nil)),
			ResourceName:  contactAddr,
			ImportState:   true,
			ImportStateId: "abc",
			ExpectError:   re(`(?s)(numeric|number).*abc`),
		},
	)
}

func TestAccDomainContact_ReadAPIErrorKeepsState(t *testing.T) {
	api := testapi.New(t)
	accTest(t,
		tfresource.TestStep{Config: contactConfig(api, nil)},
		tfresource.TestStep{
			PreConfig: func() {
				api.InjectResponse("GET", "/account/domain-contacts", 20,
					testapi.Fail(403, "INSUFFICIENT_SCOPE", "Token lacks scope domain:contact.", nil))
			},
			RefreshState: true,
			ExpectError:  re(`(?s)INSUFFICIENT_SCOPE.*domain:contact`),
		},
	)
}
