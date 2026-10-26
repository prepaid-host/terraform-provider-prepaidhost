// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

//go:build tools

package tools

import (
	// Documentation generator, run by `go generate ./...` (see main.go).
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)
