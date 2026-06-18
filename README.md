# Agent HTTP Go

This project is a Go implementation of the local Agent HTTP service from
`/Volumes/D/web/codex-http`.

It exposes installed agent CLIs through a synchronous local HTTP API. The
currently supported runnable agents are:

- `codex`, via `codex exec`.
- `claude`, via `claude --bare -p --output-format json`.

## Requirements

- Go 1.25.
- macOS or Linux.
- `codex` CLI installed and available in the service process `PATH`.
- Optional: `claude` CLI installed and available in the service process `PATH`.

## Run

```sh
/Users/grimm/sdk/go1.25.0/bin/go run ./cmd/agent-http-go
```

The server listens on `127.0.0.1:8787` by default. Override with `HOST` and
`PORT`:

```sh
HOST=127.0.0.1 PORT=8787 /Users/grimm/sdk/go1.25.0/bin/go run ./cmd/agent-http-go
```

## Endpoints

### `GET /health`

Returns:

```json
{"ok":true}
```

### `GET /agents`

Reports whether known agent CLI commands are available in `PATH`, and whether
this service supports running them through `POST /runs`.

### `POST /runs`

Runs a supported agent:

```sh
curl -sS -X POST http://127.0.0.1:8787/runs \
  -H 'Content-Type: application/json' \
  -d '{"agent":"codex","prompt":"Reply with exactly: pong"}'
```

Request fields:

- `agent`: required for `/runs`; supported values are `codex` and `claude`.
- `prompt`: required non-empty string.
- `cwd`: optional; must resolve inside the service workspace.

### `POST /codex`

Compatibility endpoint equivalent to `POST /runs` with `agent: "codex"`.

Use `debug=1` on `/runs` or `/codex` to include raw stdout and stderr:

```sh
curl -sS -X POST 'http://127.0.0.1:8787/codex?debug=1' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Reply with exactly: pong"}'
```

## Limits

- Maximum request body: 1 MiB.
- Execution timeout: 10 minutes.
- Each run starts one CLI child process.

## Test

```sh
/Users/grimm/sdk/go1.25.0/bin/go test ./...
```
