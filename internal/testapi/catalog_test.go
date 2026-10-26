// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi_test

import (
	"encoding/json"
	"testing"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

func TestCatalogShapes(t *testing.T) {
	api := testapi.New(t)

	resp, env := call(t, api, "GET", "/catalog", api.Token, nil, nil)
	if resp.StatusCode != 200 || !env.Success {
		t.Fatalf("GET /catalog: %d %+v", resp.StatusCode, env)
	}

	keys := map[string][]string{
		"rootserver": {"options", "durations", "templates"},
		"rootpacket": {"packets", "durations_months", "templates"},
		"domain":     {"tlds", "durations_months"},
	}
	for typ, want := range keys {
		resp, env := call(t, api, "GET", "/catalog/"+typ, api.Token, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", typ, resp.StatusCode)
		}
		var res map[string]json.RawMessage
		if err := json.Unmarshal(env.Result, &res); err != nil {
			t.Fatal(err)
		}
		if len(res) != len(want) {
			t.Errorf("%s: keys = %v, want %v", typ, res, want)
		}
		for _, k := range want {
			if _, ok := res[k]; !ok {
				t.Errorf("%s: missing key %q", typ, k)
			}
		}
	}
}

func TestCatalogUnknownTypeAndOverride(t *testing.T) {
	api := testapi.New(t)

	resp, env := call(t, api, "GET", "/catalog/webhosting", api.Token, nil, nil)
	if resp.StatusCode != 404 || env.Error["code"] != "UNKNOWN_TYPE" {
		t.Errorf("unknown type: %d %+v", resp.StatusCode, env.Error)
	}

	api.SetCatalog("domain", map[string]any{"tlds": []any{}, "durations_months": []any{12}})
	_, env = call(t, api, "GET", "/catalog/domain", api.Token, nil, nil)
	if string(env.Result) != `{"durations_months":[12],"tlds":[]}` {
		t.Errorf("override = %s", env.Result)
	}
}
