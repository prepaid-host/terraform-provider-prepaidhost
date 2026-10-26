// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

const (
	sshKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user@host"
	sshKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq8dQ1b3b3Zk1m4P8o1v8y0m0Zs6l9m1jQ2yq7wK5xT other@host"
)

const sshAddr = "prepaidhost_ssh_key.laptop"

func sshKeyConfig(api *testapi.Server, name, key string, extra string) string {
	return providerConfig(api) + fmt.Sprintf(`
resource "prepaidhost_ssh_key" "laptop" {
  name       = %q
  public_key = %q
%s}
`, name, key, extra)
}

func TestAccSSHKey_CreateReadImportDelete(t *testing.T) {
	api := testapi.New(t)

	accTest(t,
		resource.TestStep{
			Config: sshKeyConfig(api, "Laptop", sshKeyA, "  provision_on_reinstall = true\n"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrSet(sshAddr, "id"),
				resource.TestCheckResourceAttr(sshAddr, "name", "Laptop"),
				resource.TestCheckResourceAttr(sshAddr, "public_key", sshKeyA),
				resource.TestCheckResourceAttr(sshAddr, "provision_on_reinstall", "true"),
				resource.TestCheckResourceAttr(sshAddr, "fingerprint", testapi.SSHFingerprint(sshKeyA)),
				resource.TestCheckResourceAttrSet(sshAddr, "created_at"),
				func(*terraform.State) error {
					if n := len(api.ResourceIDs(testapi.ResourceSSHKey)); n != 1 {
						return fmt.Errorf("mock has %d keys, want 1", n)
					}
					return nil
				},
			),
		},
		// The API never returns the public key, so it cannot be verified after import.
		resource.TestStep{
			ResourceName:            sshAddr,
			ImportState:             true,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"public_key"},
		},
	)

	// terraform destroy at the end of accTest removed the key from the account.
	if n := len(api.ResourceIDs(testapi.ResourceSSHKey)); n != 0 {
		t.Errorf("%d keys left after destroy", n)
	}
	if got := api.CallCount("DELETE", "/account/ssh-keys/{id}"); got != 1 {
		t.Errorf("DELETE calls = %d, want 1", got)
	}
}

func TestAccSSHKey_ProvisionOnReinstallDefaultsToFalse(t *testing.T) {
	api := testapi.New(t)
	accTest(t, resource.TestStep{
		Config: sshKeyConfig(api, "Laptop", sshKeyA, ""),
		Check:  resource.TestCheckResourceAttr(sshAddr, "provision_on_reinstall", "false"),
	})
}

func TestAccSSHKey_ChangeReplaces(t *testing.T) {
	api := testapi.New(t)
	var firstID string

	accTest(t,
		resource.TestStep{
			Config: sshKeyConfig(api, "Laptop", sshKeyA, ""),
			Check: resource.TestCheckResourceAttrWith(sshAddr, "id", func(v string) error {
				firstID = v
				return nil
			}),
		},
		// New public key -> replace (free, so no guard needed).
		resource.TestStep{
			Config: sshKeyConfig(api, "Laptop", sshKeyB, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrWith(sshAddr, "id", func(v string) error {
					if v == firstID {
						return fmt.Errorf("id unchanged, key was not replaced")
					}
					return nil
				}),
				resource.TestCheckResourceAttr(sshAddr, "fingerprint", testapi.SSHFingerprint(sshKeyB)),
				func(*terraform.State) error {
					if n := len(api.ResourceIDs(testapi.ResourceSSHKey)); n != 1 {
						return fmt.Errorf("mock has %d keys, want 1 (old key must be deleted)", n)
					}
					return nil
				},
			),
		},
		// New name -> replace as well.
		resource.TestStep{
			Config: sshKeyConfig(api, "Desktop", sshKeyB, ""),
			Check:  resource.TestCheckResourceAttr(sshAddr, "name", "Desktop"),
		},
	)
}

func TestAccSSHKey_DeletedOutsideTerraformIsRecreated(t *testing.T) {
	api := testapi.New(t)
	var id string

	accTest(t,
		resource.TestStep{
			Config: sshKeyConfig(api, "Laptop", sshKeyA, ""),
			Check: resource.TestCheckResourceAttrWith(sshAddr, "id", func(v string) error {
				id = v
				return nil
			}),
		},
		resource.TestStep{
			PreConfig: func() {
				n, _ := strconv.ParseInt(id, 10, 64)
				api.DeleteResource(testapi.ResourceSSHKey, n)
			},
			// Refresh drops the key from state; the plan wants to create it again.
			RefreshState:       true,
			ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: sshKeyConfig(api, "Laptop", sshKeyA, ""),
			Check: resource.TestCheckResourceAttrWith(sshAddr, "id", func(v string) error {
				if v == id {
					return fmt.Errorf("key was not recreated")
				}
				return nil
			}),
		},
	)
}

func TestAccSSHKey_DestroyToleratesMissingKey(t *testing.T) {
	api := testapi.New(t)
	accTest(t, resource.TestStep{
		Config: sshKeyConfig(api, "Laptop", sshKeyA, ""),
		Check: func(*terraform.State) error {
			// Gone before destroy: the DELETE answers 404 (and the key is still
			// listed, so Read does not drop it first in this test).
			api.InjectResponse("DELETE", "/account/ssh-keys/{id}", 1,
				testapi.Fail(404, "NOT_FOUND", "Endpoint nicht gefunden.", nil))
			return nil
		},
	})
	if got := api.CallCount("DELETE", "/account/ssh-keys/{id}"); got != 1 {
		t.Errorf("DELETE calls = %d, want 1", got)
	}
}

func TestAccSSHKey_CreateAPIErrors(t *testing.T) {
	api := testapi.New(t)
	api.SeedSSHKey("existing", sshKeyA)

	accTest(t,
		resource.TestStep{
			Config:      sshKeyConfig(api, "Laptop", "not a key", ""),
			ExpectError: re(`(?s)VALIDATION_ERROR.*public_key`),
		},
		resource.TestStep{
			Config:      sshKeyConfig(api, "Laptop", sshKeyA, ""),
			ExpectError: re(`DUPLICATE_KEY`),
		},
	)
	if n := len(api.ResourceIDs(testapi.ResourceSSHKey)); n != 1 {
		t.Errorf("failed creates must not add keys, have %d", n)
	}
}

func TestAccSSHKey_ImportThenAdoptPublicKeyWithoutReplace(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedSSHKey("Laptop", sshKeyA)
	idStr := strconv.FormatInt(id, 10)

	accTest(t,
		initStep(api),
		resource.TestStep{
			Config:             importConfig(sshKeyConfig(api, "Laptop", sshKeyA, "")),
			ResourceName:       sshAddr,
			ImportState:        true,
			ImportStateId:      idStr,
			ImportStatePersist: true,
		},
		// public_key is null after import; applying the config fills it in place
		// (no replace, no write call) because the fingerprint matches.
		resource.TestStep{
			Config: sshKeyConfig(api, "Laptop", sshKeyA, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(sshAddr, "id", idStr),
				resource.TestCheckResourceAttr(sshAddr, "public_key", sshKeyA),
				func(*terraform.State) error {
					if got := api.WriteCount(); got != 0 {
						return fmt.Errorf("adopting the public key made %d write calls", got)
					}
					return nil
				},
			),
		},
	)
}

func TestAccSSHKey_ImportWithWrongPublicKeyFails(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedSSHKey("Laptop", sshKeyA)

	accTest(t,
		initStep(api),
		resource.TestStep{
			Config:             importConfig(sshKeyConfig(api, "Laptop", sshKeyB, "")),
			ResourceName:       sshAddr,
			ImportState:        true,
			ImportStateId:      strconv.FormatInt(id, 10),
			ImportStatePersist: true,
		},
		resource.TestStep{
			Config:      sshKeyConfig(api, "Laptop", sshKeyB, ""),
			ExpectError: re(`(?s)public_key.*(does not match|fingerprint)`),
		},
	)
}

func TestAccSSHKey_ImportUnknownID(t *testing.T) {
	api := testapi.New(t)
	accTest(t,
		resource.TestStep{
			Config:        importConfig(sshKeyConfig(api, "Laptop", sshKeyA, "")),
			ResourceName:  sshAddr,
			ImportState:   true,
			ImportStateId: "4711",
			ExpectError:   re(`(?s)(SSH key not found|No SSH key).*4711`),
		},
		resource.TestStep{
			Config:        importConfig(sshKeyConfig(api, "Laptop", sshKeyA, "")),
			ResourceName:  sshAddr,
			ImportState:   true,
			ImportStateId: "abc",
			ExpectError:   re(`(?s)(numeric|number).*abc`),
		},
	)
}
