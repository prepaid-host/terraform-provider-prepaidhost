# Contributing

Thanks for helping with the Prepaid-Host provider. Bugs and ideas: open an issue. Code: open a pull request against `main`.

## Setup

- Go (version in `go.mod`), OpenTofu or Terraform for the acceptance-style tests, `golangci-lint`.
- `make build`, `make lint`, `make test`, `make testacc` (see the README).

## Rules

- **Tests run against the mock API** (`internal/testapi`), never against the real API: no test may spend balance. The mock models the server's behaviour; when the server changes, change the mock too.
- Every change needs a test. `go vet ./...`, `golangci-lint run` and `go test -race ./...` must pass, also with `TF_ACC=1` (`make testacc`).
- **The provider must never buy silently.** Anything that can order or charge needs a quote, a `max_price` check and a deterministic idempotency key. A changed purchase argument is a plan error, not a replacement.
- Sensitive values: mark them `Sensitive`; use write-only attributes where the value must not reach the state.
- No uptime or SLA promises in documentation or descriptions.

## Documentation

`docs/` is generated; do not edit it by hand. Change the schema descriptions in `internal/provider/*.go`, the examples in `examples/` or the templates in `templates/`, then run:

```sh
go generate ./...
```

and commit the result. Examples must be valid for Terraform and OpenTofu (`source = "prepaid-host/prepaidhost"`).

## Releases

Maintainers only, see [RELEASING.md](RELEASING.md). Add a line to `CHANGELOG.md` under *Unreleased* for user-visible changes.

## License

Contributions are licensed under the [MPL-2.0](LICENSE).
