// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points terraform-plugin-testing at the registry host of the CLI in
// use: OpenTofu resolves providers at registry.opentofu.org, Terraform at
// registry.terraform.io (the plugin's default).
func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC_PROVIDER_HOST") == "" &&
		strings.HasPrefix(filepath.Base(os.Getenv("TF_ACC_TERRAFORM_PATH")), "tofu") {
		_ = os.Setenv("TF_ACC_PROVIDER_HOST", "registry.opentofu.org")
	}
	os.Exit(m.Run())
}
