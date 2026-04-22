# Contributing

Thanks for your interest in Caffeine! This is a small, opinionated
project — here's how to make a change land smoothly.

## Before you start

- **Open an issue first** for anything non-trivial (new feature,
  refactor, behaviour change). A few lines of back-and-forth before
  code saves everyone time.
- Typo fixes, doc tweaks, and small bugfixes: just send a PR.

## Getting set up

See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for the full dev
environment. TL;DR:

```bash
make dev-mock                                   # fake Meticulous on :8090
MACHINE_URL=http://localhost:8090 make dev-api  # Go API on :8080
make dev-web                                    # Vite on :5173
```

You don't need a real Meticulous machine — `cmd/mockmachine` covers
every code path.

## Before you push

```bash
make fmt           # go fmt
make test          # go test ./...
cd web && npx tsc --noEmit && npm run build
```

CI runs the same checks; failing any of them will block the PR.

## PR guidelines

- **One thing per PR.** Separate refactors from behaviour changes.
- **Keep diffs tight.** Don't reformat unrelated files or add
  drive-by changes.
- **Commit messages** — imperative mood, short subject, meaningful
  body explaining *why*. Something like:
  ```
  feat(ai): persist coach suggestions per (shot, model)

  Re-opening a shot was re-billing the LLM every time because coach
  output only lived in the POST handler. Mirror the analysis cache:
  …
  ```
- **New features that touch the machine or an AI provider** should
  include a smoke test using `mockmachine` or a fake HTTP transport —
  we don't want CI hitting real providers.
- **UI changes** — please include a screenshot in the PR description.
- **No backwards-incompatible config changes** without a deprecation
  note in the PR description and a reasonable migration path.

## Style

- Go — standard `gofmt`; prefer short, well-named functions over
  deeply nested logic; comments explain *why*, not *what*.
- TypeScript — project settings win (`tsconfig.json`, Tailwind); no
  bikeshedding.
- SQL schema changes — always `CREATE TABLE IF NOT EXISTS` /
  `ALTER TABLE … ADD COLUMN IF NOT EXISTS`. Users upgrade in place.

## Security

If you find a security issue, **please don't open a public issue.**
Email the maintainer or use GitHub's private vulnerability reporting
on this repository.

## Licence of contributions

By submitting a PR you agree that your contribution is licensed under
the same [MIT Licence](LICENSE) as the rest of the project.
