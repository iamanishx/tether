# tether

A small, durable control layer for AI agents that run code in sandboxes. Works with
[E2B](https://e2b.dev) (hosted) and [Perplexity SPACE](https://github.com/perplexityai/SPACE) (self-hosted).

The sandbox gives you isolation. Your agent (Pi, or anything that can call HTTP) gives you reasoning.
**tether sits between them and makes the *actions* durable**: what was requested, what ran, and what
is still unknown after a crash.

```
agent (Pi, etc.) ──HTTP──▶ tetherd ──┬──▶ E2B sandbox      (-backend e2b)
                             │       └──▶ SPACE microVM    (-backend space)
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

Requires Go 1.23+. Pick a backend; the ledger, API and recovery rules are identical for both.

**E2B** (hosted, zero ops; needs an [API key](https://e2b.dev/dashboard)):

```bash
export E2B_API_KEY=e2b_...
export TETHER_API_TOKEN=$(openssl rand -hex 24)   # bearer token for the tether API
make build
./bin/tetherd -backend e2b -data-dir ./data
```

**SPACE** (self-hosted Firecracker microVMs; needs a running [SPACE deployment](https://github.com/perplexityai/SPACE/blob/main/docs/installation.md)):

```bash
export SPACE_API_URL=https://space.internal          # control plane origin
export SPACE_DATA_PLANE_URL=https://space-data.internal
export SPACE_API_TOKEN=...                           # management bearer token from `space-admin-cli login`
export TETHER_API_TOKEN=$(openssl rand -hex 24)
make build
./bin/tetherd -backend space -data-dir ./data        # -template defaults to seed:latest
```

### Backend differences

| | `e2b` | `space` |
| :-- | :-- | :-- |
| Where it runs | E2B cloud | Your Kubernetes / KVM hosts |
| Execution | Jupyter-style code interpreter (stateful kernel, rich results) | One process per action (`python3 -c`, `sh -c`, `node -e`); no kernel state between actions |
| Languages | python, bash, javascript, r, java | python, bash, javascript |
| Idle handling | Auto-pause after `-sandbox-ttl`, resumed on next action | SPACE's own idle policy (default: suspend after 6 h); tether wakes it on next action |
| Lifecycle calls | Synchronous | Async operations, polled by tether |
| Output | NDJSON stream from the sandbox | SSE from the data plane with a per-command attachment token |
| Non-zero exit | n/a (exceptions are reported) | Recorded as `exit_code` + an `ExitError` on the action (a *failed* action, not an unknown one) |

On SPACE, every mutation carries a fresh `Idempotency-Key`, and the attachment token is only ever sent to
`SPACE_DATA_PLANE_URL`, never to a host the API response points at.

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

Language defaults to Python; pass `"language": "bash"` or `"javascript"` for others (E2B also accepts `r` and `java`).

## Operating notes

- **One owning process per data dir** (enforced with `flock`). Back up `data/` or put it on a replicated volume;
  tether fsyncs every write, but a lost disk is a lost ledger.
- Keep **artifacts you care about outside the sandbox**. `complete`/`cancel` kill the sandbox.
- Idle sandboxes are paused/suspended, not killed, and resumed on the next action (E2B: `-sandbox-ttl`; SPACE: its own idle policy).
- A crash between "sandbox created" and "sandbox id saved" can orphan one paused sandbox. They are tagged with
  `tether_task_id` metadata in E2B so you can find and reap them.
- Treat anything fetched by sandbox code (web pages, PDFs) as untrusted input.

## Status

Unit and HTTP tests cover the ledger, recovery, approvals and reconciliation using a fake sandbox
(`make test`). The SPACE adapter is tested against an in-process fake of SPACE's control and data planes.
Both adapters are written against the providers' published OpenAPI specs and reference clients, but
**neither has yet been run against a live deployment** — try each with a throwaway key / dev cluster first.

## Layout

```
cmd/tetherd           daemon entrypoint and flags
internal/api          HTTP/JSON routes and auth
internal/task         task model, state machine, engine (ledger, approvals, recovery)
internal/store        atomic, fsynced, single-owner JSON store
internal/sandbox      Sandbox interface, E2B adapter, SPACE adapter, in-memory fake for tests
```
