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

### 2. Terraform Registry (registry.terraform.io)

1. In the Registry, sign in with GitHub. The account needs the OAuth access to the `prepaid-host` organisation (an organisation admin may have to approve the Registry app once).
2. Add the **public key**: User Settings, Signing Keys, choose the **`prepaid-host` namespace**, paste `prepaidhost-public.asc`. Only an organisation admin can add keys to the namespace.
3. Publish, Provider, choose organisation `prepaid-host` and the repository `terraform-provider-prepaidhost` (public, name must match `terraform-provider-{NAME}`, lower case).
4. The Registry creates a webhook on the repository for release events and ingests every new release from it. If new versions stop appearing: delete the old `registry.terraform.io` webhooks of the repository and use **Resync** on the provider's settings page.

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
