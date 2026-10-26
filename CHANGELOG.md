# Changelog

All notable changes to this provider. Versions follow [Semantic Versioning](https://semver.org). The release process is in [RELEASING.md](RELEASING.md).

## 0.1.0 (October 26, 2026)

First release.

FEATURES:

* **New Resource:** `prepaidhost_ssh_key`
* **New Resource:** `prepaidhost_domain_contact`
* **New Resource:** `prepaidhost_vserver` (root packets and custom servers; destroy cancels without refund)
* **New Resource:** `prepaidhost_domain` (register; transfers are not available through the API)
* **New Resource:** `prepaidhost_dns_record`
* **New Data Source:** `prepaidhost_catalog`
* **New Data Source:** `prepaidhost_balance`

NOTES:

* Creating a `prepaidhost_vserver` or `prepaidhost_domain` charges the prepaid balance. `max_price` caps the order total; a repeated `apply` after an abort does not buy twice.
* `destroy` cancels (auto-renew off, runs until `expires_at`, no refund). The same configuration applied again before expiry takes the resource over without a new purchase.
* Works with Terraform and OpenTofu (`source = "prepaid-host/prepaidhost"`).
* The API's rate limit (HTTP 429) is waited out for reads, quotes and orders, following `Retry-After`; other writes are not repeated.
* The provider checks the endpoint once when it starts and fails with a clear error if it does not answer like the Prepaid-Host API, so a wrong `endpoint` never removes resources from the state.
