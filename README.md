# Terraform / OpenTofu Provider for Prepaid-Host

The `prepaid-host/prepaidhost` provider manages [Prepaid-Host](https://prepaid-host.com) resources with Terraform or OpenTofu: SSH keys, domain contacts, vServers, domains and DNS records. Orders are paid from the prepaid balance of your account.

> **Status:** in development, not yet published in the Terraform or OpenTofu Registry.

## Usage

```hcl
terraform {
  required_providers {
    prepaidhost = {
      source = "prepaid-host/prepaidhost"
    }
  }
}

provider "prepaidhost" {
  # token    = "..."                            # or PREPAIDHOST_TOKEN
  # endpoint = "https://prepaid-host.com/api"   # or PREPAIDHOST_ENDPOINT
}
```

| Argument   | Environment variable   | Description |
|------------|------------------------|-------------|
| `token`    | `PREPAIDHOST_TOKEN`    | Personal API Bearer token from the customer panel (sensitive). Ordering needs a token created with the order scopes. |
| `endpoint` | `PREPAIDHOST_ENDPOINT` | API base URL, default `https://prepaid-host.com/api`. |

### Billing behaviour

- Creating a vServer or domain **charges your balance immediately**. The provider asks for a quote first and orders with an idempotency key, so a repeated `apply` after an interruption does not buy twice.
- `destroy` **cancels** a vServer or domain: auto-renew is switched off and the resource runs until its paid period ends. There is no refund.
- Changing a purchase-relevant argument (e.g. memory, package, duration) is rejected at plan time instead of silently buying a replacement.

## Development

Requirements: Go (see `go.mod`), OpenTofu or Terraform, optionally `golangci-lint`.

```sh
make build      # go build ./...
make lint       # golangci-lint run
make test       # unit tests
make testacc    # acceptance-style tests with the tofu/terraform binary
```

All tests run against an in-process mock of the API (`internal/testapi`); no test talks to the real API or spends balance. `make testacc` sets `TF_ACC=1`, `TF_ACC_TERRAFORM_PATH` (tofu, else terraform) and `TF_ACC_PROVIDER_NAMESPACE=prepaid-host`.

Layout:

- `internal/client` — API client (envelope handling, `APIError`, retries, orders).
- `internal/testapi` — mock API used by all tests.
- `internal/provider` — provider, resources and data sources.

## License

[MPL-2.0](LICENSE)
