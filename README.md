# tether

A small, durable control layer for AI agents that run code in sandboxes. Adapters for
[E2B Cloud](https://e2b.dev), self-hosted [E2B Embed](https://github.com/e2b-dev/runtime/tree/main/embed)
and self-hosted [Perplexity SPACE](https://github.com/perplexityai/SPACE).

The sandbox gives you isolation. Your agent (Pi, or anything that can call HTTP) gives you reasoning.
**tether sits between them and makes the *actions* durable**: what was requested, what ran, and what
is still unknown after a crash.

```
agent (Pi, etc.) ──HTTP──▶ tetherd ──┬──▶ E2B Cloud or E2B Embed   (-backend e2b)
                             │       └──▶ SPACE microVM            (-backend space)
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

## Local E2B setup

On a supported Linux host with Docker Compose, KVM, a 4 KiB-page kernel and sufficient RAM/disk:

```bash
make local-doctor  # read-only prerequisite checks
make local-up      # provision real E2B Embed, build Tether, discover credentials automatically
make e2e           # run the live guest/ledger checks; currently exposes a known resume-capacity failure
make local-down    # stop services; retain data and E2B host setup
make local-purge   # delete the owned Embed demo databases and undo its host setup
```

This is a **local development setup, not a production-ready or fully passing E2E distribution**.
No Pi installation, model subscription or E2B cloud account is needed for setup. Model/React benchmarking
is not yet implemented. Platform containers run E2B's control plane; code executes in Firecracker guests,
not ordinary Docker containers.

The helper defaults to a 512 MiB hugepage pool, one active action, and pause-after-action. It reserves
memory before starting databases, disables PostgreSQL's competing hugepage use, and blocks non-loopback
traffic to internal ports. The official privileged host setup loads kernel modules and modifies NBD,
hugepages, sysctls and networking. `local-down` retains these host changes; use `local-purge` to undo the
owned demo setup. Store credentials, ledgers and diagnostics under git-ignored `.local/`; generated
credential/configuration files are private. Do not commit or publish them.

See [the test report](docs/TESTING.md) for what is verified and what still fails.

## Run it manually

Requires Go 1.23+. Pick a backend; the ledger, API and recovery rules are identical for both.

**E2B** (hosted, zero ops; needs an [API key](https://e2b.dev/dashboard)):

```bash
export E2B_API_KEY=e2b_...
export TETHER_API_TOKEN=$(openssl rand -hex 24)   # bearer token for the tether API
make build
./bin/tetherd -backend e2b -data-dir ./data
```

**E2B Embed** (self-hosted, same `-backend e2b`; the whole E2B stack on one Linux host with KVM, via
Docker Compose, or Terraform for GCP / AWS / Azure, or one Kubernetes node — see the
[Embed guide](https://github.com/e2b-dev/runtime/blob/main/embed/README.md)). After `docker compose up`,
Embed prints the SDK variables; tether reads the same names:

```bash
eval "$(docker compose exec ready cat /run/e2b/sdk.env)"   # sets E2B_API_KEY, E2B_API_URL, E2B_SANDBOX_URL, ...
export TETHER_API_TOKEN=$(openssl rand -hex 24)
./bin/tetherd -backend e2b -template base -e2b-exec-mode commands -data-dir ./data
```

`E2B_API_URL` is the REST API (port 3000) and `E2B_SANDBOX_URL` the client-proxy (port 3002), which
routes to a sandbox by `E2b-Sandbox-Id` / `E2b-Sandbox-Port` headers. Leave both unset for E2B Cloud.
With self-hosted URL overrides, the default executor is `commands`, using envd on port 49983 and
plain templates such as `base`. Each action is a fresh process; files are the place for persistent state.
Python/Node must exist in the selected template. Set `-e2b-exec-mode interpreter` (or `E2B_EXEC_MODE`)
when a template includes the code-interpreter server on port 49999. Unset URL overrides keep E2B Cloud's
interpreter default and `code-interpreter-v1` template.

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

| | `e2b` (Cloud) | `e2b` (Embed, self-hosted) | `space` |
| :-- | :-- | :-- | :-- |
| Where it runs | E2B cloud | One Linux host with KVM you own (Compose / Terraform GCP, AWS, Azure / one K8s node) | Your Kubernetes / KVM hosts |
| Ops burden | Provider-managed | This helper operates one node; a multi-node runtime is a separate deployment | High: control plane, btrfs nodes, warm pools, object storage |
| Execution | Interpreter by default; commands optional | envd process execution by default; interpreter optional | One process per action (`python3 -c`, `sh -c`, `node -e`); no kernel state between actions |
| Languages | Interpreter: python, bash, javascript, r, java | Commands: python, bash/sh, javascript, if installed in the template | python, bash, javascript |
| Idle handling | Auto-pause after `-sandbox-ttl`, resumed on next action | Auto-pause after `-sandbox-ttl`, resumed on next action | SPACE's own idle policy (default: suspend after 6 h); tether wakes it on next action |
| Lifecycle calls | Synchronous | Synchronous | Async operations, polled by tether |
| Output | Interpreter: NDJSON; commands: framed Connect JSON | Commands: framed Connect JSON with stdout/stderr and process ID | SSE from the data plane with a per-command attachment token |
| Non-zero exit | Interpreter exceptions; commands report `exit_code` + `ExitError` | Confirmed process exits are failures, not unknown outcomes | Recorded as `exit_code` + an `ExitError` on the action (a *failed* action, not an unknown one) |

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

Language defaults to Python; pass `"language": "bash"` or `"javascript"` for others. E2B's interpreter mode
also accepts `r` and `java`; command mode supports only Python, Bash/sh and JavaScript and requires the
corresponding executable in the template.

## Operating notes

- **Self-hosted E2B Embed:** its REST API (3000) and client-proxy (3002) authenticate only by the team API
  key; firewall them to tether's host, and never expose the orchestrator's unauthenticated gRPC port (5008).
- **One owning process per data dir** (enforced with `flock`). Back up `data/` or put it on a replicated volume;
  tether fsyncs every write, but a lost disk is a lost ledger.
- Keep **artifacts you care about outside the sandbox**. `complete`/`cancel` kill the sandbox.
- Idle sandboxes are paused/suspended, not killed, and resumed on the next action (E2B: `-sandbox-ttl`; SPACE: its own idle policy).
- `-max-concurrent-actions` limits in-flight execution in this daemon; capacity waits share the action deadline.
  `-pause-after-action` requests a pause before releasing its slot, and records pause success/failure.
  Guest memory reclamation is not instantaneous; the local live test still exposes a resume-capacity failure.
- Action IDs bind code, language, approval policy and replay safety. Changed requests return a conflict.
- Interrupted envd streams retain available process IDs/output as evidence, without claiming successful execution.
- A crash between "sandbox created" and "sandbox id saved" can orphan one paused sandbox. They are tagged with
  `tether_task_id` metadata in E2B so you can find and reap them.
- Treat anything fetched by sandbox code (web pages, PDFs) as untrusted input.

## Status

`make test` covers the ledger, approvals, recovery, bounded execution, envd framing and local configuration.

- **E2B Embed: partially live-tested.** Real guests executed initial commands and action deduplication passed.
  The full pause/resume and filesystem verification did **not** pass: reconnecting a paused guest hit a
  memory-pool allocation error before the filesystem read could execute. This is not evidence of a data leak.
- **E2B Cloud: not live-tested in this session.**
- **SPACE: adapter/protocol tests only, not tested against a live SPACE deployment.**
- **No React-agent benchmark or validated maximum agent count yet.**

See [docs/TESTING.md](docs/TESTING.md). Do not treat provider adapters or the local helper as production-certified.

## Layout

```
cmd/tetherd           daemon entrypoint and flags
internal/api          HTTP/JSON routes and auth
internal/task         task model, state machine, engine (ledger, approvals, recovery)
internal/store        atomic, fsynced, single-owner JSON store
internal/sandbox      Sandbox interface, E2B adapter, SPACE adapter, in-memory fake for tests
```
