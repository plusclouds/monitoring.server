# Contributing

Thanks for helping. Bug reports, plugin ideas, docs fixes and code are all welcome.

## Ways to contribute

- **Report a bug or ask for a feature:** open an [issue](https://github.com/plusclouds/monitoring.server/issues/new/choose). Include the version (`monitor --version`), what you expected and what happened.
- **Ask a question or share a setup:** use GitHub Discussions.
- **Add a plugin:** protocols and device types are plugins, never core changes. Start from `pkg/plugin` and an existing plugin such as `plugins/http`, and read the [feature specs](docs/features/README.md).
- **Change the design:** edit the relevant feature spec or add an ADR from the [template](docs/adr/0000-template.md) in the same pull request.
- **Security issues:** do not open a public issue. Email security@plusclouds.com.

Look for issues labelled `good first issue`.

## Development

Requirements: Docker; Go 1.27 to build and test from source. See [Development in the README](README.md#development) to run the stack.

```sh
make build   # bin/monitor
make test    # unit and integration tests with -race (needs Docker)
make lint    # golangci-lint, including gosec
make check   # everything CI runs
```

If you change `api/openapi.yaml`, run `make generate` and commit the result.

## Pull requests

- Branch from `master`, one topic per pull request.
- Use conventional commit prefixes (`feat:`, `fix:`, `docs:`, `ci:`).
- Add or update tests; `make check` must pass.
- Update the docs and [CHANGELOG.md](CHANGELOG.md) for user-visible changes.
- Dependencies must use a license on the allowlist (see `ALLOWED_LICENSES` in the Makefile).

By contributing you agree your work is released under the [MIT license](LICENSE).
