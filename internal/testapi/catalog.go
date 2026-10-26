// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"net/http"
	"slices"
)

// catalogTypes are the types GET /catalog lists.
var catalogTypes = []string{"rootserver", "rootpacket", "domain"}

// GET /catalog and GET /catalog/{type} like the API. The shape of each type's
// result follows the API's catalog of that type.
func init() {
	register(func(s *Server) {
		s.Handle(http.MethodGet, "/catalog", handleCatalogTypes)
		s.Handle(http.MethodGet, "/catalog/{type}", handleCatalog)
	})
}

func handleCatalogTypes(_ *State, _ *Request) Response {
	return OK(map[string]any{"types": catalogTypes})
}

func handleCatalog(st *State, r *Request) Response {
	typ := r.Param("type")
	if !slices.Contains(catalogTypes, typ) {
		return Fail(http.StatusNotFound, "UNKNOWN_TYPE", "Unknown product type \""+typ+"\".", nil)
	}
	if c, ok := st.Catalogs[typ]; ok {
		return OK(c)
	}
	return OK(defaultCatalog(typ))
}

// SetCatalog replaces the result of GET /catalog/{type}.
func (s *Server) SetCatalog(typ string, result map[string]any) {
	s.With(func(st *State) {
		if st.Catalogs == nil {
			st.Catalogs = map[string]map[string]any{}
		}
		st.Catalogs[typ] = result
	})
}

func templates() []any {
	return []any{
		map[string]any{"id": 11, "name": "Debian 12", "os_family": "linux"},
		map[string]any{"id": 12, "name": "Windows Server 2022", "os_family": "windows"},
	}
}

// defaultCatalog is a small but complete catalog in the exact shape of the
// API's catalog (option values are strings, as the API sends them).
func defaultCatalog(typ string) map[string]any {
	switch typ {
	case "rootserver":
		opt := func(value, name string, price float64) any {
			return map[string]any{"value": value, "name": name, "price_monthly": price}
		}
		return map[string]any{
			"options": map[string]any{
				"cores":     []any{opt("1", "1 vCore", 1.5), opt("2", "2 vCores", 3)},
				"memory":    []any{opt("1024", "1 GB", 1), opt("2048", "2 GB", 2)},
				"disk":      []any{opt("20", "20 GB", 1), opt("40", "40 GB", 2)},
				"addresses": []any{opt("1", "1 IPv4", 0.5)},
				"backups":   []any{opt("0", "No backups", 0), opt("1", "1 backup", 1)},
				"traffic":   []any{opt("unlimited", "Unlimited", 0)},
			},
			"durations": []any{
				map[string]any{"days": 30, "months": 1, "discount_percent": 0.0},
				map[string]any{"days": 90, "months": 3, "discount_percent": 2.5},
				map[string]any{"days": 180, "months": 6, "discount_percent": 5.0},
				map[string]any{"days": 365, "months": 12, "discount_percent": 10.0},
			},
			"templates": templates(),
		}
	case "rootpacket":
		return map[string]any{
			"packets": []any{
				map[string]any{
					"id": 1, "name": "Start", "cores": 1, "memory": 1024, "disk": 20,
					"addresses": 1, "backups": 1, "traffic": "unlimited",
					"price_monthly": 4.99, "active_promo_percent": 0.0,
				},
				map[string]any{
					"id": 2, "name": "Plus", "cores": 2, "memory": 2048, "disk": 40,
					"addresses": 1, "backups": 2, "traffic": "unlimited",
					"price_monthly": 9.99, "active_promo_percent": 15.0,
				},
			},
			"durations_months": []any{1, 3, 6, 12},
			"templates":        templates(),
		}
	default: // domain
		return map[string]any{
			"tlds": []any{
				map[string]any{"tld": "com", "price_yearly": 11.99, "price_yearly_discounted": nil},
				map[string]any{"tld": "de", "price_yearly": 7.5, "price_yearly_discounted": 4.99},
			},
			"durations_months": []any{12, 24, 36},
		}
	}
}
