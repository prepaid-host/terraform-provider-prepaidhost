// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Rootserver endpoints the vServer resource uses, each answering exactly like
// the real one — including the older shapes:
//
//   - GET  /rootserver/{id}              envelope; unknown id → HTTP 200 {"success":false,"result":"No Entrys available."}
//   - GET  /rootserver/{id}/rental       bare object {created_rental, delete_rental}; unknown → 404 {"success":false,"result":"Server not found."}
//   - GET  /rootserver/{id}/addresses    bare array [{id,address,gateway,netmask}]; unknown → 404 NOT_FOUND envelope
//   - GET  /rootserver/{id}/renew        envelope {balance, expires_at, auto_renew, options}; unknown → NOT_FOUND
//   - PUT  /rootserver/{id}/display-name {"display_name"} 4–32 chars; 422 VALIDATION_ERROR; unknown → NOT_FOUND
//   - PUT  /rootserver/{id}/autorenew    {"enabled"} boolean; 422 VALIDATION_ERROR; unknown → NOT_FOUND
//
// Rootserver attributes in State.Resources["rootserver"]: cores, memory,
// disk, addresses, backups, traffic, template_id, packet_id, installed
// (bool), display_name, auto_renew (bool), created_at / expires_at (RFC 3339),
// ip_addresses ([]map[string]any) and install_after_reads (mock only).
func init() {
	register(func(s *Server) {
		s.Handle(http.MethodGet, "/rootserver/{id}", handleShowRootserver)
		s.Handle(http.MethodGet, "/rootserver/{id}/rental", handleRootserverRental)
		s.Handle(http.MethodGet, "/rootserver/{id}/addresses", handleRootserverAddresses)
		s.Handle(http.MethodGet, "/rootserver/{id}/renew", handleRootserverRenewInfo)
		s.Handle(http.MethodPut, "/rootserver/{id}/display-name", handleRootserverDisplayName)
		s.Handle(http.MethodPut, "/rootserver/{id}/autorenew", handleRootserverAutoRenew)
	})
}

// renewIntervals are the renewal periods in days the API offers.
var renewIntervals = []int{10, 30, 90, 180, 365}

// apiTime is the API's timestamp format (microseconds, UTC).
const apiTime = "2006-01-02T15:04:05.000000Z"

func abort404() Response {
	return Fail(http.StatusNotFound, "NOT_FOUND", "Endpoint nicht gefunden.", nil)
}

func (st *State) rootserver(r *Request) (map[string]any, bool) {
	id, ok := r.ParamInt64("id")
	if !ok {
		return nil, false
	}
	return st.Resource(ResourceRootserver, id)
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(math.Round(x))
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func handleShowRootserver(st *State, r *Request) Response {
	attrs, ok := st.rootserver(r)
	if !ok {
		return Legacy(http.StatusOK, "No Entrys available.")
	}
	if n, ok := attrs["install_after_reads"].(int); ok && n > 0 {
		n--
		attrs["install_after_reads"] = n
		if n == 0 {
			attrs["installed"] = true
		}
	}
	// MySQL tinyint without cast: 0/1.
	installed := 0
	if b, _ := attrs["installed"].(bool); b {
		installed = 1
	}
	return OK(map[string]any{
		"id":            attrs["id"],
		"cores":         toInt(attrs["cores"]),
		"memory":        toInt(attrs["memory"]),
		"disk_size":     toInt(attrs["disk"]),
		"addresses":     toInt(attrs["addresses"]),
		"installed":     installed,
		"backup_amount": toInt(attrs["backups"]),
		"template_id":   toInt(attrs["template_id"]),
		"packet_id":     toInt(attrs["packet_id"]),
	})
}

func apiDate(v any) any {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return t.UTC().Format(apiTime)
}

func handleRootserverRental(st *State, r *Request) Response {
	attrs, ok := st.rootserver(r)
	if !ok {
		return Legacy(http.StatusNotFound, "Server not found.")
	}
	return Raw(http.StatusOK, map[string]any{
		"created_rental": apiDate(attrs["created_at"]),
		"delete_rental":  apiDate(attrs["expires_at"]),
	})
}

func handleRootserverAddresses(st *State, r *Request) Response {
	attrs, ok := st.rootserver(r)
	if !ok {
		return abort404()
	}
	list, _ := attrs["ip_addresses"].([]map[string]any)
	out := make([]map[string]any, 0, len(list))
	out = append(out, list...)
	return Raw(http.StatusOK, out)
}

func handleRootserverRenewInfo(st *State, r *Request) Response {
	attrs, ok := st.rootserver(r)
	if !ok {
		return abort404()
	}
	monthly := st.Prices["rootserver"]
	options := make([]map[string]any, 0, len(renewIntervals))
	for _, days := range renewIntervals {
		price := round2(monthly * float64(days) / 30)
		options = append(options, map[string]any{"interval": days, "price": price, "affordable": st.Balance >= price})
	}
	var expires any
	if t, err := time.Parse(time.RFC3339, fmt.Sprint(attrs["expires_at"])); err == nil {
		expires = t.UTC().Format("2006-01-02T15:04:05-07:00")
	}
	autoRenew, _ := attrs["auto_renew"].(bool)
	return OK(map[string]any{
		"balance":    round2(st.Balance),
		"expires_at": expires,
		"auto_renew": autoRenew,
		"options":    options,
	})
}

func validationError(field, message string) Response {
	return Fail(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Validierungsfehler.",
		map[string]any{"details": map[string]any{field: []string{message}}})
}

func handleRootserverDisplayName(st *State, r *Request) Response {
	attrs, ok := st.rootserver(r)
	if !ok {
		return abort404()
	}
	name, isString := r.BodyMap()["display_name"].(string)
	switch {
	case !isString || name == "":
		return validationError("display_name", "The display name field is required.")
	case len([]rune(name)) < 4:
		return validationError("display_name", "The display name field must be at least 4 characters.")
	case len([]rune(name)) > 32:
		return validationError("display_name", "The display name field must not be greater than 32 characters.")
	}
	attrs["display_name"] = name
	return OK(map[string]any{"display_name": name})
}

// apiBoolean is what the API accepts as a boolean: true, false, 1, 0, "1", "0".
func apiBoolean(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case float64:
		if x == 0 || x == 1 {
			return x == 1, true
		}
	case string:
		if x == "0" || x == "1" {
			return x == "1", true
		}
	}
	return false, false
}

func handleRootserverAutoRenew(st *State, r *Request) Response {
	attrs, ok := st.rootserver(r)
	if !ok {
		return abort404()
	}
	v, present := r.BodyMap()["enabled"]
	if !present || v == nil {
		return validationError("enabled", "The enabled field is required.")
	}
	enabled, ok := apiBoolean(v)
	if !ok {
		return validationError("enabled", "The enabled field must be true or false.")
	}
	attrs["auto_renew"] = enabled
	return OK(map[string]any{"auto_renew": enabled})
}

// packet returns a packet of the rootpacket catalog in use.
func (st *State) packet(id int64) (map[string]any, bool) {
	c, ok := st.Catalogs["rootpacket"]
	if !ok {
		c = defaultCatalog("rootpacket")
	}
	packets, _ := c["packets"].([]any)
	for _, p := range packets {
		if m, ok := p.(map[string]any); ok && toInt(m["id"]) == id {
			return m, true
		}
	}
	return nil, false
}

// fulfillRootserver completes a rootserver created by an order the way the
// API does: packet specs for a packet, packet_id 0 for a
// custom server, display_name = hostname, not yet installed, and the IPs.
func (st *State) fulfillRootserver(itemType string, attrs map[string]any) {
	packetID := int64(0)
	if itemType == "rootpacket" {
		packetID = toInt(attrs["packet_id"])
		if p, ok := st.packet(packetID); ok {
			for _, k := range []string{"cores", "memory", "disk", "addresses", "backups", "traffic"} {
				attrs[k] = p[k]
			}
		}
	}
	attrs["packet_id"] = packetID
	if host, ok := attrs["hostname"].(string); ok && host != "" {
		attrs["display_name"] = host
	}
	attrs["installed"] = false
	attrs["ip_addresses"] = st.newAddresses(int(toInt(attrs["addresses"])))
}

// newAddresses hands out n IPv4 addresses (TEST-NET-3) plus one IPv6
// address, like a server's address list.
func (st *State) newAddresses(n int) []map[string]any {
	out := make([]map[string]any, 0, n+1)
	for i := 0; i < n; i++ {
		st.nextIP++
		out = append(out, map[string]any{
			"id": st.nextIP, "address": fmt.Sprintf("203.0.113.%d", st.nextIP%250+1),
			"gateway": "203.0.113.254", "netmask": "255.255.255.0",
		})
	}
	st.nextIP++
	out = append(out, map[string]any{
		"id": st.nextIP, "address": fmt.Sprintf("2001:db8::%x", st.nextIP),
		"gateway": "2001:db8::1", "netmask": "64",
	})
	return out
}

// SeedRootserver stores a live, installed rootserver (custom, 2 cores,
// 2048 MB, 40 GB, 1 IPv4, 1 backup, template 11, expires in 30 days,
// auto-renew on) with attrs overriding the defaults, and returns its id.
func (s *Server) SeedRootserver(attrs map[string]any) int64 {
	var id int64
	s.With(func(st *State) {
		now := time.Now().UTC()
		a := map[string]any{
			"cores": 2, "memory": 2048, "disk": 40, "addresses": 1, "backups": 1, "traffic": "unlimited",
			"template_id": 11, "packet_id": int64(0), "installed": true, "auto_renew": true,
			"created_at": now.Format(time.RFC3339), "expires_at": now.AddDate(0, 0, 30).Format(time.RFC3339),
		}
		for k, v := range attrs {
			a[k] = v
		}
		if _, ok := a["ip_addresses"]; !ok {
			a["ip_addresses"] = st.newAddresses(int(toInt(a["addresses"])))
		}
		id = st.AddResource(ResourceRootserver, a)
	})
	return id
}

// SetInstallAfterReads makes a rootserver report installed only from the
// n-th GET /rootserver/{id} on (e.g. a taken-over server still installing).
func (s *Server) SetInstallAfterReads(id int64, n int) {
	s.UpdateResource(ResourceRootserver, id, func(attrs map[string]any) {
		attrs["installed"] = n <= 0
		attrs["install_after_reads"] = n
	})
}

// validateServerItem checks what the API checks beyond the request
// validation, as far as the mock models it: the duration values and, for a
// packet, that the packet exists (rejected with one "field").
func (st *State) validateServerItem(index int, itemType string, it map[string]any) *Response {
	invalid := func(key, message string) *Response {
		resp := invalidField(fmt.Sprintf("items.%d.%s", index, key), message)
		return &resp
	}
	switch itemType {
	case "rootserver":
		if v, ok := it["duration_days"]; ok {
			switch toInt(v) {
			case 30, 90, 180, 365:
			default:
				return invalid("duration_days", "The selected duration days is invalid.")
			}
		}
	case "rootpacket":
		if v, ok := it["duration_months"]; ok {
			switch toInt(v) {
			case 1, 3, 6, 12:
			default:
				return invalid("duration_months", "The selected duration months is invalid.")
			}
		}
		if _, ok := st.packet(toInt(it["packet_id"])); !ok {
			return invalid("packet_id", "Ungültiges Paket.")
		}
	}
	return nil
}
