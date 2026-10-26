// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi_test

import (
	"encoding/json"
	"testing"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user@host"

func TestSSHKeys_CreateListDelete(t *testing.T) {
	api := testapi.New(t)

	resp, env := call(t, api, "POST", "/account/ssh-keys", api.Token,
		map[string]any{"name": "Laptop", "public_key": testKey, "provision_on_reinstall": true}, nil)
	if resp.StatusCode != 201 || !env.Success {
		t.Fatalf("create: %d %+v", resp.StatusCode, env)
	}
	var key map[string]any
	if err := json.Unmarshal(env.Result, &key); err != nil {
		t.Fatal(err)
	}
	// Exactly the fields of AccountBillingController::storeSshKey — no public_key echoed.
	for _, k := range []string{"id", "name", "fingerprint", "provision_on_reinstall", "created_at"} {
		if _, ok := key[k]; !ok {
			t.Errorf("missing %q in %v", k, key)
		}
	}
	if len(key) != 5 {
		t.Errorf("unexpected fields: %v", key)
	}
	if key["fingerprint"] != testapi.SSHFingerprint(testKey) {
		t.Errorf("fingerprint = %v", key["fingerprint"])
	}

	_, env = call(t, api, "GET", "/account/ssh-keys", api.Token, nil, nil)
	var list []map[string]any
	_ = json.Unmarshal(env.Result, &list)
	if len(list) != 1 || list[0]["name"] != "Laptop" {
		t.Fatalf("list = %v", list)
	}

	id := int64(key["id"].(float64)) //nolint:forcetypeassert // test
	resp, env = call(t, api, "DELETE", "/account/ssh-keys/"+itoa(id), api.Token, nil, nil)
	if resp.StatusCode != 200 || !env.Success {
		t.Fatalf("delete: %d %+v", resp.StatusCode, env)
	}
	resp, env = call(t, api, "DELETE", "/account/ssh-keys/"+itoa(id), api.Token, nil, nil)
	if resp.StatusCode != 404 || env.Success || env.Error["code"] != "NOT_FOUND" {
		t.Errorf("second delete: %d %+v", resp.StatusCode, env)
	}
}

func TestSSHKeys_Errors(t *testing.T) {
	api := testapi.New(t)

	resp, env := call(t, api, "POST", "/account/ssh-keys", api.Token,
		map[string]any{"name": "x", "public_key": "not a key"}, nil)
	if resp.StatusCode != 422 || env.Error["code"] != "VALIDATION_ERROR" {
		t.Errorf("invalid key: %d %+v", resp.StatusCode, env)
	}
	if _, ok := env.Error["details"].(map[string]any)["public_key"]; !ok {
		t.Errorf("details = %v", env.Error["details"])
	}

	call(t, api, "POST", "/account/ssh-keys", api.Token, map[string]any{"name": "a", "public_key": testKey}, nil)
	resp, env = call(t, api, "POST", "/account/ssh-keys", api.Token, map[string]any{"name": "b", "public_key": testKey}, nil)
	if resp.StatusCode != 409 || env.Error["code"] != "DUPLICATE_KEY" {
		t.Errorf("duplicate: %d %+v", resp.StatusCode, env)
	}
}
