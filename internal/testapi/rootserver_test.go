// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/testapi"
)

// rawGET returns the decoded body of an endpoint that answers without envelope.
func rawGET(t *testing.T, api *testapi.Server, path string) (int, any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, api.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+api.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var v any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&v); err != nil {
		t.Fatalf("%s: invalid JSON %q", path, raw)
	}
	return resp.StatusCode, v
}

func placeServer(t *testing.T, api *testapi.Server, it map[string]any) int64 {
	t.Helper()
	api.SetBalance(100)
	host, _ := it["hostname"].(string)
	resp, env := call(t, api, http.MethodPost, "/orders", api.Token,
		map[string]any{"items": []any{it}, "expected_total": 9.99}, map[string]string{"Idempotency-Key": "key-order-" + host})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("order: %d %+v", resp.StatusCode, env)
	}
	var order struct {
		ID    string `json:"id"`
		Items []struct {
			ResourceID int64 `json:"resource_id"`
		} `json:"items"`
	}
	_ = json.Unmarshal(env.Result, &order)
	return order.Items[0].ResourceID
}

func customItem(host string) map[string]any {
	return map[string]any{
		"type": "rootserver", "cores": 2, "memory": 2048, "disk": 40, "addresses": 1, "backups": 1,
		"traffic": "unlimited", "duration_days": 90, "template_id": 11, "hostname": host, "client_reference": host,
	}
}

// GET /rootserver/{id} has exactly the columns RootserverController::show selects.
func TestRootserver_ShowAfterOrder(t *testing.T) {
	api := testapi.New(t)
	id := placeServer(t, api, customItem("web1"))

	_, env := call(t, api, http.MethodGet, "/rootserver/"+itoa(id), api.Token, nil, nil)
	if !env.Success {
		t.Fatalf("show: %+v", env)
	}
	var show map[string]any
	_ = json.Unmarshal(env.Result, &show)
	want := map[string]any{"id": float64(id), "cores": 2.0, "memory": 2048.0, "disk_size": 40.0, "addresses": 1.0,
		"installed": 0.0, "backup_amount": 1.0, "template_id": 11.0, "packet_id": 0.0}
	if len(show) != len(want) {
		t.Errorf("fields = %v", show)
	}
	for k, v := range want {
		if show[k] != v {
			t.Errorf("%s = %v, want %v", k, show[k], v)
		}
	}

	// Polling the order until active marks the server installed (the API derives
	// the item status from vServer::installed).
	for _, oid := range api.OrderIDs() {
		call(t, api, http.MethodGet, "/orders/"+oid, api.Token, nil, nil)
		call(t, api, http.MethodGet, "/orders/"+oid, api.Token, nil, nil)
	}
	_, env = call(t, api, http.MethodGet, "/rootserver/"+itoa(id), api.Token, nil, nil)
	_ = json.Unmarshal(env.Result, &show)
	if show["installed"] != 1.0 {
		t.Errorf("installed = %v after provisioning", show["installed"])
	}
}

func TestRootserver_PacketTakesPacketSpecs(t *testing.T) {
	api := testapi.New(t)
	id := placeServer(t, api, map[string]any{"type": "rootpacket", "packet_id": 2, "duration_months": 1, "template_id": 11, "hostname": "pk1"})
	r, _ := api.Resource(testapi.ResourceRootserver, id)
	if r["cores"] != 2 || r["memory"] != 2048 || r["packet_id"] != int64(2) || r["display_name"] != "pk1" {
		t.Errorf("packet server = %v", r)
	}

	api.SetBalance(100)
	resp, env := call(t, api, http.MethodPost, "/orders/quote", api.Token,
		map[string]any{"items": []any{map[string]any{"type": "rootpacket", "packet_id": 99, "duration_months": 1, "template_id": 11}}}, nil)
	if resp.StatusCode != 422 || env.Error["field"] != "items.0.packet_id" {
		t.Errorf("unknown packet: %d %+v", resp.StatusCode, env)
	}
}

func TestRootserver_NotFoundShapes(t *testing.T) {
	api := testapi.New(t)

	resp, env := call(t, api, http.MethodGet, "/rootserver/4711", api.Token, nil, nil)
	if resp.StatusCode != 200 || env.Success || string(env.Result) != `"No Entrys available."` {
		t.Errorf("show: %d %+v", resp.StatusCode, env)
	}
	resp, env = call(t, api, http.MethodGet, "/rootserver/4711/rental", api.Token, nil, nil)
	if resp.StatusCode != 404 || string(env.Result) != `"Server not found."` {
		t.Errorf("rental: %d %+v", resp.StatusCode, env)
	}
	for _, p := range []string{"/rootserver/4711/addresses", "/rootserver/4711/renew"} {
		resp, env = call(t, api, http.MethodGet, p, api.Token, nil, nil)
		if resp.StatusCode != 404 || env.Error["code"] != "NOT_FOUND" {
			t.Errorf("%s: %d %+v", p, resp.StatusCode, env)
		}
	}
	for _, p := range []string{"/rootserver/4711/display-name", "/rootserver/4711/autorenew"} {
		resp, env = call(t, api, http.MethodPut, p, api.Token, map[string]any{"display_name": "abcd", "enabled": true}, nil)
		if resp.StatusCode != 404 || env.Error["code"] != "NOT_FOUND" {
			t.Errorf("%s: %d %+v", p, resp.StatusCode, env)
		}
	}
}

func TestRootserver_RentalAddressesRenew(t *testing.T) {
	api := testapi.New(t)
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	id := api.SeedRootserver(map[string]any{"expires_at": expires.Format(time.RFC3339), "addresses": 1})

	status, v := rawGET(t, api, "/rootserver/"+itoa(id)+"/rental")
	m, _ := v.(map[string]any)
	if status != 200 || m["delete_rental"] != "2030-01-02T03:04:05.000000Z" || m["created_rental"] == nil || len(m) != 2 {
		t.Errorf("rental: %d %v", status, v)
	}

	status, v = rawGET(t, api, "/rootserver/"+itoa(id)+"/addresses")
	list, _ := v.([]any)
	if status != 200 || len(list) != 2 {
		t.Fatalf("addresses: %d %v", status, v)
	}
	first, _ := list[0].(map[string]any)
	for _, k := range []string{"id", "address", "gateway", "netmask"} {
		if _, ok := first[k]; !ok {
			t.Errorf("address lacks %s: %v", k, first)
		}
	}

	_, env := call(t, api, http.MethodGet, "/rootserver/"+itoa(id)+"/renew", api.Token, nil, nil)
	var renew map[string]any
	_ = json.Unmarshal(env.Result, &renew)
	if renew["expires_at"] != "2030-01-02T03:04:05+00:00" || renew["auto_renew"] != true {
		t.Errorf("renew = %v", renew)
	}
	if opts, _ := renew["options"].([]any); len(opts) != 5 {
		t.Errorf("options = %v", renew["options"])
	}
}

func TestRootserver_DisplayNameAndAutoRenew(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedRootserver(nil)

	resp, env := call(t, api, http.MethodPut, "/rootserver/"+itoa(id)+"/display-name", api.Token, map[string]any{"display_name": "abc"}, nil)
	if resp.StatusCode != 422 || env.Error["code"] != "VALIDATION_ERROR" {
		t.Errorf("short name: %d %+v", resp.StatusCode, env)
	}
	resp, env = call(t, api, http.MethodPut, "/rootserver/"+itoa(id)+"/display-name", api.Token, map[string]any{"display_name": "web-new"}, nil)
	if resp.StatusCode != 200 || string(env.Result) != `{"display_name":"web-new"}` {
		t.Errorf("rename: %d %s", resp.StatusCode, env.Result)
	}
	if r, _ := api.Resource(testapi.ResourceRootserver, id); r["display_name"] != "web-new" {
		t.Errorf("display_name = %v", r["display_name"])
	}

	resp, env = call(t, api, http.MethodPut, "/rootserver/"+itoa(id)+"/autorenew", api.Token, map[string]any{"enabled": "yes"}, nil)
	if resp.StatusCode != 422 || env.Error["code"] != "VALIDATION_ERROR" {
		t.Errorf("invalid enabled: %d %+v", resp.StatusCode, env)
	}
	resp, env = call(t, api, http.MethodPut, "/rootserver/"+itoa(id)+"/autorenew", api.Token, map[string]any{"enabled": false}, nil)
	if resp.StatusCode != 200 || string(env.Result) != `{"auto_renew":false}` {
		t.Errorf("autorenew: %d %s", resp.StatusCode, env.Result)
	}
	if r, _ := api.Resource(testapi.ResourceRootserver, id); r["auto_renew"] != false {
		t.Errorf("auto_renew = %v", r["auto_renew"])
	}
}

// SetInstallAfterReads: a server that finishes installing after n reads.
func TestRootserver_InstallAfterReads(t *testing.T) {
	api := testapi.New(t)
	id := api.SeedRootserver(map[string]any{"installed": false})
	api.SetInstallAfterReads(id, 2)
	var got []any
	for i := 0; i < 3; i++ {
		_, env := call(t, api, http.MethodGet, "/rootserver/"+itoa(id), api.Token, nil, nil)
		var show map[string]any
		_ = json.Unmarshal(env.Result, &show)
		got = append(got, show["installed"])
	}
	if got[0] != 0.0 || got[1] != 1.0 || got[2] != 1.0 {
		t.Errorf("installed over reads = %v", got)
	}
}
