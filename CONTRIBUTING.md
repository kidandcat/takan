# Contributing

Thanks for considering a contribution.

## Development

Requirements: recent Go (see `go.mod`), network only for tests that need it (most unit tests are offline).

```bash
export TAKAN_PUBLIC_URL=http://127.0.0.1:8090
export TAKAN_SESSION_KEY=$(openssl rand -hex 32)
go test ./...
go run ./cmd/takan
```

Panel: open `TAKAN_PUBLIC_URL`. First registered user becomes admin.

## Pull requests

- Keep **code, comments, commits, and PR text in English**.
- Prefer small, focused PRs (one module or one concern).
- Run `go test ./...` before opening the PR.
- Do not commit `data/`, secrets, or real credentials.
- Match existing style: multi-tenant by `user_id`, no trust of client-supplied user IDs in tools.

## Modules

New integrations live under `modules/<name>/` and register in `modules/registry.go` + `store.defaultModuleIDs`. Prefer a small MCP tool surface; readiness belongs in `takan_status`, not a per-module `*_status` tool.

## Security

See [SECURITY.md](SECURITY.md) for vulnerability reports — do not file public issues for those.
