# tether

A small, durable control layer for AI agents that run code in [E2B](https://e2b.dev) sandboxes.

E2B gives you isolated sandboxes. Your agent (Pi, or anything that can call HTTP) gives you reasoning.
**tether sits between them and makes the *actions* durable**: what was requested, what ran, and what
is still unknown after a crash.

```
agent (Pi, etc.) ──HTTP──▶ tetherd ──▶ E2B sandbox (code-interpreter)
                             │
                             └── durable task + action ledger on disk
```

## What it guarantees

| Problem | What tether does |
| :-- | :-- |
| Agent retries a request and the action runs twice | Every action has a caller-chosen `action_id`. Re-submitting it returns the recorded result and **never re-executes**. |
| Process dies mid-action | Intent is fsynced *before* the sandbox call. On restart, idempotent actions are replayed (bounded); all others become `outcome_unknown`. |
| Response lost after the action took effect | The action is `outcome_unknown`, not "failed". The task cannot complete until someone reconciles it (`resolve`). |
| High-impact action | `requires_approval` parks it in `pending_approval` until approved or rejected, across restarts. |
| "The agent says it's done" | `complete` refuses while any action is unresolved and requires evidence for success. |
| Sandbox disappears | A new one is created and a `sandbox_lost` event is recorded, so you know in-sandbox state is gone. |
| Runaway loops | `max_attempts` and `max_actions` are persisted in the ledger, not in the model's memory. |

It does **not** promise exactly-once effects on external systems; nothing local can. For consequential
calls use the external service's own idempotency key, and use `resolve` to reconcile unknown outcomes.
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for diagrams and the full state machine.

## Run it

Requires Go 1.23+ and an [E2B API key](https://e2b.dev/dashboard).

```bash
export E2B_API_KEY=e2b_...
export TETHER_API_TOKEN=$(openssl rand -hex 24)   # bearer token for the tether API
make build
./bin/tetherd -data-dir ./data -listen 127.0.0.1:8090
```

## API

All routes except `/healthz` need `Authorization: Bearer $TETHER_API_TOKEN`.

```bash
H="Authorization: Bearer $TETHER_API_TOKEN"

# 1. create a task (idempotent on id)
curl -X POST localhost:8090/v1/tasks -H "$H" -d '{"id":"q3-report","title":"Summarise PDFs"}'

# 2. run an action; the same action_id never runs twice
curl -X POST localhost:8090/v1/tasks/q3-report/actions -H "$H" -d '{
  "action_id": "parse-1",
  "code": "import pypdf; print(len(pypdf.PdfReader(\"/home/user/a.pdf\").pages))",
  "idempotent": true
}'

# 3. a high-impact action waits for approval (HTTP 202)
curl -X POST localhost:8090/v1/tasks/q3-report/actions -H "$H" -d '{
  "action_id": "publish-1", "code": "publish()", "requires_approval": true }'
curl -X POST localhost:8090/v1/tasks/q3-report/actions/publish-1/approve -H "$H" -d '{"note":"ok"}'

# 4. reconcile an outcome_unknown action: succeeded | failed | retry
curl -X POST localhost:8090/v1/tasks/q3-report/actions/publish-1/resolve -H "$H" \
  -d '{"outcome":"succeeded","note":"checked the site, post is live"}'

# 5. finish (needs evidence; kills the sandbox)
curl -X POST localhost:8090/v1/tasks/q3-report/complete -H "$H" \
  -d '{"state":"succeeded","evidence":"report.pdf, 12 pages, uploaded to s3://..."}'
```

Other routes: `GET /v1/tasks`, `GET /v1/tasks/{id}`, `POST /v1/tasks/{id}/cancel`,
`POST /v1/tasks/{id}/actions/{aid}/reject`.

Language defaults to Python; pass `"language": "bash"` (or `javascript`, `r`, `java`) for others.

## Operating notes

- **One owning process per data dir** (enforced with `flock`). Back up `data/` or put it on a replicated volume;
  tether fsyncs every write, but a lost disk is a lost ledger.
- Keep **artifacts you care about outside the sandbox**. `complete`/`cancel` kill the sandbox.
- Sandboxes auto-pause (not die) when idle (`-sandbox-ttl`), and are resumed on the next action.
- A crash between "sandbox created" and "sandbox id saved" can orphan one paused sandbox. They are tagged with
  `tether_task_id` metadata in E2B so you can find and reap them.
- Treat anything fetched by sandbox code (web pages, PDFs) as untrusted input.

## Status

Unit and HTTP tests cover the ledger, recovery, approvals and reconciliation using a fake sandbox
(`make test`). The E2B adapter is written against E2B's published OpenAPI spec and the code-interpreter
server source, but **has not yet been run against a live E2B account** — try it with a throwaway key first.

## Layout

```
cmd/tetherd           daemon entrypoint and flags
internal/api          HTTP/JSON routes and auth
internal/task         task model, state machine, engine (ledger, approvals, recovery)
internal/store        atomic, fsynced, single-owner JSON store
internal/sandbox      Sandbox interface, E2B adapter, in-memory fake for tests
```
