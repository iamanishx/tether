# Test status and next acceptance gates

Status recorded from local testing on 2026-11-04. This is not a production certification.

## What Tether is

Tether is an agent-independent, durable task/action gateway. Pi, OpenCode or an application can submit
HTTP requests. E2B or SPACE provides the isolated machine. Tether owns the fsynced action ledger,
deduplication, approvals, bounded execution, crash classification and explicit reconciliation.
It is not a model host, complete agent framework, Firecracker implementation, or distributed scheduler.

## Verified

- Go tests, the Go race detector, `go vet`, Python configuration tests and Python syntax checks pass.
- A non-root Tether Docker image builds.
- Local E2B preflight, runtime setup and base-template build completed.
- Two distinct live E2B sandboxes executed their initial shell actions. They reported guest kernel
  `6.1.177+` while the host ran `7.0.0-38-generic`.
- Re-submitting each initial action with the same ID returned the existing receipt without another
  attempt.
- The free DeepSeek model worked in an earlier **ordinary Docker smoke test**. That result is not an
  E2B agent benchmark and is not counted as one.

## Not verified / known failure

The complete live test (`make e2e`) did **not** pass. The first paused sandbox could not reconnect:

> Failed to place sandbox: this node cannot map 512 MiB of guest memory: its memory pool is too small
> or in use by running sandboxes.

The subsequent filesystem read was not dispatched. An earlier test message called this a
"cross-guest filesystem leak"; that was an incorrect classification, now fixed. No filesystem leak
was demonstrated. Pause/resume persistence and the full Firecracker descriptor check remain unverified.

- E2B Cloud was not live-tested in this session.
- SPACE has in-process adapter/protocol tests, but no live deployment test.
- No React-agent tasks, browser acceptance tests, throughput benchmark or maximum-agent measurement.
- One active action is a configured local limit, not a measured hardware maximum.

## Setup findings

1. Initial registry downloads were slow and hit deadlines. Later retries downloaded all runtime images.
2. Host memory fragmentation prevented a 1 GiB hugepage pool despite ordinary available RAM. A 512 MiB
   pool could be reserved after stopping the test services.
3. PostgreSQL's hugepage use competed with the VM pool. Local setup now disables it and reserves guest
   memory before starting databases.
4. Resolved Compose configurations retained upstream volume/network names. The helper now assigns
   owned names so independent installations do not silently share databases.
5. The cold base-template build took about four minutes, in addition to runtime artifact downloads.

## Required before a proper E2B E2E pass

1. Add bounded lifecycle capacity waiting/retry for reconnecting the **same sandbox ID**. Do not replay
   the action code just because lifecycle placement failed. Inspect pause failures and memory
   reclamation; measure whether the configured pool can actually support the intended resident guests.
2. Pass real guest execution, stable-ID dedupe, same-path filesystem isolation and write/pause/resume/read
   persistence. Observe a live Firecracker process with KVM descriptors rather than relying on Docker
   container counts or an agent's statement.
3. Inject real daemon crashes and response loss: unsafe actions must remain unknown, block completion,
   and require reconciliation; explicitly replay-safe actions must obey the attempt bound.
4. Test concurrent duplicate requests, approval/rejection over restarts, deadline handling, cancellation,
   deliberate sandbox deletion and the `sandbox_lost` event. A replaced sandbox must not be presented
   as if old files were recovered.
5. Run the intended model inside real guests, give separate agents React tasks, and independently verify
   builds and browser behavior. Report time, tokens/cost, rate limiting, peak resident guests, memory,
   CPU, disk, and the distinction between queued logical agents and resident VMs.
6. Repeat on the intended AWS/Azure KVM-capable host. Kubernetes is not needed for these correctness
   gates; multi-node placement/HA should come after the single-host path passes.

## Commands and safety

- `make test`: offline Go and Python tests; does not start VMs.
- `make local-doctor`: read-only host checks.
- `make local-up`: actual privileged E2B setup plus automatic private credential wiring and Tether startup.
- `make e2e`: live verification, currently expected to expose the capacity issue above on this small pool.
- `make local-down`: stop the owned containers; retain data, templates and host setup.
- `make local-purge`: remove the owned Embed databases and undo its host setup; retain Tether's ledger/reports.

Generated credentials and raw runtime reports live under git-ignored `.local/`; do not publish them.
Cold setup needs outbound access to image registries, runtime artifacts and the template's package
repositories. Docker/KVM/compatible kernel/RAM/disk remain prerequisites, not hidden dependencies.
A standalone React benchmark command is intentionally not shipped until implemented and verified.
