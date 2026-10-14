# Repository instructions

## Go code style

Read and follow [CODE_STYLE_GO.md](CODE_STYLE_GO.md) before writing, modifying,
or reviewing Go code in this repository. It is the shared code style reference
for implementation and tests. Follow its rule priority when requirements conflict.

- Preserve the requirements in [DESAFIO.md](DESAFIO.md), public contracts, and
  deliberate package conventions.
- Keep changes focused, use the standard library where practical, and keep
  domain code independent of frameworks and infrastructure.
- Check returned errors and preserve useful error causes with `%w`.
- Make Go code easy to scan with one blank line between logical blocks, including
  consecutive guard clauses. Keep calls together with their error checks. Apply
  the vertical-spacing rules in `CODE_STYLE_GO.md` to implementation and tests.
- Use descriptive table-driven tests for behavior, invariants, and boundaries.
- Keep each condition focused on one rule; split unrelated validations into
  guard clauses and use `switch` for lists of allowed states or kinds.
- Split long function signatures into one parameter per line, each with its own
  type and a trailing comma, following `CODE_STYLE_GO.md`.
- Format changed Go files with `gofmt`. Run `go test ./...` and `go vet ./...`;
  also run `go test -race ./...` for the challenge's applicable tests. Report
  checks that could not run.

## Modular monolith boundaries

- Keep `internal/app` focused on composing modules and process-wide settings.
- Keep financial use cases, domain, persistence, HTTP adapters, and workers in
  `internal/wagering`; preserve their shared SQL transaction boundary.
- Keep reusable infrastructure in `internal/platform`. Platform packages must
  not import business modules.
- Business modules expose their composition through their root `module.go`.
  Production packages outside a business module must not import its adapters.
- Register HTTP routes through the server's contribution group. Keep listener
  ownership and shutdown in `platform/httpserver`.
- Keep domain packages independent of Fx, transports, and persistence libraries.
- Move tests with their packages and preserve `TestModuleBoundaries`.
