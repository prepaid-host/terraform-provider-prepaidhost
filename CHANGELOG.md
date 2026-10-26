# Changelog

All notable changes to this provider. Versions follow [Semantic Versioning](https://semver.org). The release process is in [RELEASING.md](RELEASING.md).

## Unreleased

Nothing is published yet; the first release will contain:

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
