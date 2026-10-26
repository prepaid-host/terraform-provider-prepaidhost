// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/client"
)

// addAPIError adds err to diags as an error diagnostic. API errors show their
// HTTP status, code and message plus, for validation errors, the offending
// fields; anything else (network error, ...) is shown as is.
func addAPIError(diags *diag.Diagnostics, summary string, err error) {
	diags.AddError(summary, describeError(err))
}

func describeError(err error) string {
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		return err.Error()
	}
	var b strings.Builder
	b.WriteString(apiErr.Error())
	if client.IsThrottled(err) {
		b.WriteString("\n\nThe API's rate limit was reached. Wait a minute and run again; " +
			"fewer parallel operations (-parallelism) keep below the limit.")
	}
	for _, key := range []string{"fields", "details"} {
		if v, ok := apiErr.Extra[key]; ok && v != nil {
			b.WriteString("\n\n")
			b.WriteString(key)
			b.WriteString(":\n")
			b.WriteString(formatFields(v))
		}
	}
	return b.String()
}

// formatFields renders the API's validation error map ({"field": ["message", ...]}) one field per line.
func formatFields(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		switch msgs := m[k].(type) {
		case []any:
			parts := make([]string, 0, len(msgs))
			for _, msg := range msgs {
				parts = append(parts, fmt.Sprint(msg))
			}
			lines = append(lines, fmt.Sprintf("  %s: %s", k, strings.Join(parts, "; ")))
		default:
			lines = append(lines, fmt.Sprintf("  %s: %v", k, msgs))
		}
	}
	return strings.Join(lines, "\n")
}
