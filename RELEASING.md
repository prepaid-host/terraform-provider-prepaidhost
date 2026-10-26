# Releasing

How the `prepaid-host/prepaidhost` provider is published to the Terraform Registry and the OpenTofu Registry. A release is triggered **only** by pushing a tag `vX.Y.Z`; see `.github/workflows/release.yml` and `.goreleaser.yml`.

Sources (read them again before the first release, the requirements change):

- Terraform Registry: <https://developer.hashicorp.com/terraform/registry/providers/publishing>
- OpenTofu Registry: <https://github.com/opentofu/registry#readme> and the inclusion/immutability policy <https://github.com/opentofu/registry/blob/main/POLICY.md>
- Provider scaffolding (source of the GoReleaser config and workflow): <https://github.com/hashicorp/terraform-provider-scaffolding-framework>

## Before the first release

- **The ordering API must be live in production first.** Ordering (`POST /orders/quote`, `POST /orders`, `GET /orders`) and the other endpoints the provider uses must be deployed on <https://prepaid-host.com/api>, and an order token with consent must be creatable in the customer panel. A published version cannot be withdrawn (see the OpenTofu policy below), so a provider released before its API exists stays broken forever.
- **The one-time setup below must be complete before the first tag** — signing key and both secrets in GitHub, the provider added in the Terraform Registry. The tag is the release: without the secrets the workflow fails at the key import, and the Terraform Registry only ingests releases published after the provider was connected there.
- **Rehearse in a private fork, not here.** Do a dress rehearsal of the whole pipeline in a private fork (or a throwaway repository) with a throwaway GPG key and its own secrets: tag `v0.0.1` there, check that the release is published with all assets and that the signature verifies (`gpg --verify ..._SHA256SUMS.sig ..._SHA256SUMS`). Do **not** rehearse with an `-rc` tag in this repository: a pre-release is a release too, and OpenTofu indexes it as an immutable version. Delete the throwaway key afterwards.

## One-time setup

### 1. Signing key (shared by both registries)

Both registries verify the detached signature of `..._SHA256SUMS` with a public key you register with them.

1. Create the key. It must be **RSA** (or DSA): the Terraform Registry rejects the default ECC key type.

   ```sh
   gpg --full-generate-key     # choose "RSA and RSA", 4096 bits, no expiry (or a long one), use a team address
   gpg --list-secret-keys --keyid-format=long
   ```

2. Export both halves (ASCII-armored):

   ```sh
   gpg --armor --export <KEY_ID>             > prepaidhost-public.asc
   gpg --armor --export-secret-keys <KEY_ID> > prepaidhost-private.asc   # secret! delete after step 3
   ```

3. Store the **private key** and its **passphrase** as repository secrets in GitHub (Settings, Secrets and variables, Actions): `GPG_PRIVATE_KEY` (content of the private `.asc`) and `PASSPHRASE`. GoReleaser in CI cannot answer a passphrase prompt, which is why the workflow imports the key with these secrets. Keep a backup of the private key in the team password manager; delete the loose file.

### 2. Terraform Registry (via HCP Terraform public namespaces)

Public providers are published through a public namespace in HCP Terraform; after that the namespace is managed there, not on registry.terraform.io.

1. Sign up at <https://app.terraform.io> (free). **Do not choose the EU region** — it has no public namespaces. Create an organisation; as its creator you are in the Owners team, which is required.
2. Install the **Terraform Cloud** GitHub App on the GitHub organisation `prepaid-host` (needs organisation-admin rights on GitHub; granting it access to all repositories is simplest, at least to `terraform-provider-prepaidhost`).
3. In HCP Terraform: your organisation, Registry, Public namespaces, **New Namespace**, select the GitHub account `prepaid-host` (if it is missing, use "Add GitHub Account" to reinstall the app), **Create Namespace** ("Claim Namespace" if it was used on the Registry before).
4. In the namespace, add the **public key** (`prepaidhost-public.asc`) as signing key. Without it, signature verification fails when publishing. It must be the key the releases are signed with (RSA, not ECC).
5. In the namespace: Publish, Provider, choose `terraform-provider-prepaidhost`, pick a category, accept the terms, **Publish provider**. HCP lists every version from the repository with its status; every new GitHub release is picked up automatically from then on.
6. If a version is missing: in the namespace, on the provider, "…", **Resync**. A signature error almost always means an ECC key or a different key in the namespace than the one used for signing.
7. Never replace a published version — tag a new one. Deleting is only possible within 48 h or while a version has at most 100 downloads.

Docs can be previewed before a release at <https://registry.terraform.io/tools/doc-preview>.

### 3. OpenTofu Registry (search.opentofu.org)

The OpenTofu Registry does not host anything; it indexes the GitHub releases. Provider and signing key are submitted **separately**, **only through the issue forms** in <https://github.com/opentofu/registry> (their README: no pull requests with registry data, and issues created with the `gh` CLI, the API or other tooling are closed without processing).

1. Submit the key with the form **"Submit new Provider Signing Key"**: <https://github.com/opentofu/registry/issues/new?assignees=&labels=provider-key%2Csubmission&projects=&template=provider_key.yml&title=Provider+Key%3A+> (the public key from step 1).
2. Submit the provider with the form **"Submit new Provider"**: <https://github.com/opentofu/registry/issues/new?assignees=&labels=provider%2Csubmission&projects=&template=provider.yml&title=Provider%3A+> (`prepaid-host/terraform-provider-prepaidhost`).
3. The OpenTofu team reviews and approves or denies. After approval the provider shows up on <https://search.opentofu.org> and `tofu init` resolves `prepaid-host/prepaidhost`.

Their policy (<https://github.com/opentofu/registry/blob/main/POLICY.md>) matters for us: **a published version is immutable** and is only ever removed in exceptional cases (malware, leaked secrets, compromised key) by a TSC vote. A bad release is fixed by a new version, never by moving or re-publishing a tag. They recommend enabling GitHub's *immutable releases* for the repository.

## Every release

1. Make sure `main` is green (CI: build, lint, unit and mock-API acceptance tests with Terraform and OpenTofu).
2. Regenerate the docs and check that nothing changes: `go generate ./...` and `git status`.
3. Update `CHANGELOG.md`: move the *Unreleased* entries under a new `## X.Y.Z (YYYY-MM-DD)` heading.
4. Commit that on `main` and push.
5. Tag and push the tag (the tag is the release; **never move or overwrite a tag that was pushed**, there must also be no branch with the same name):

   ```sh
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

   Pre-releases use a hyphen: `vX.Y.Z-rc1` — they are published and immutable like any other version, so they are no way to test the pipeline (use the private fork, see above). The workflow refuses a tag whose commit is not on `main`, and runs the tests once more before it builds.
6. Watch the **Release** action. It must end green and the GitHub release must be **published** (not a draft) with these assets: the zips `terraform-provider-prepaidhost_X.Y.Z_{os}_{arch}.zip`, `..._SHA256SUMS`, `..._SHA256SUMS.sig` (binary signature), `..._manifest.json` (protocol 6.0).
7. Check the registry pages: the new version at <https://registry.terraform.io/providers/prepaid-host/prepaidhost> (takes a few minutes; Resync if it does not appear) and, once the provider is approved there, on <https://search.opentofu.org>.
8. Smoke test in an empty directory, with both tools:

   ```hcl
   terraform {
     required_providers {
       prepaidhost = {
         source  = "prepaid-host/prepaidhost"
         version = "X.Y.Z"
       }
     }
   }
   ```

   ```sh
   terraform init && terraform providers
   tofu init && tofu providers
   ```

   Do not run `apply` for the smoke test: it would order and charge.

## Local check of the pipeline (no publishing)

```sh
goreleaser check
GPG_FINGERPRINT=dummy goreleaser release --snapshot --clean --skip=sign
```

`--snapshot` never publishes and needs no tag. The result is in `dist/` (ignored by git).

## If a release goes wrong

Do not delete and re-create the tag, and do not edit assets: the registries cache checksums and users get checksum errors. Fix forward with the next patch version. If a release leaks a secret or contains malware, contact the registries (Terraform Registry support; for OpenTofu a *Version Removal Request* issue) and rotate the affected secret first.
