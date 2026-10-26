// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/prepaid-host/terraform-provider-prepaidhost/internal/provider"
)

// Generate the registry documentation (docs/) from the provider schema, the
// templates/ and the examples/. Needs a terraform or tofu binary on the PATH.
//
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name prepaidhost --rendered-provider-name "Prepaid-Host"

// version is set by goreleaser (https://goreleaser.com/cookbooks/using-main.version/).
var version = "dev"

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/prepaid-host/prepaidhost",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
