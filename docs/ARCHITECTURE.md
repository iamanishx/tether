# Architecture

tether is deliberately small. E2B owns isolation and the sandbox lifecycle; the agent owns reasoning.
tether owns exactly one thing: **a durable record of what the agent asked the sandbox to do.**

## Components

```mermaid
flowchart LR
    Agent["Agent / orchestrator<br/>(Pi, Temporal worker, ...)"]
    subgraph tetherd["tetherd (single owner of data dir)"]
        API["HTTP API<br/>bearer auth"]
        Engine["Task engine<br/>ledger · approvals · recovery"]
        Store[("Durable store<br/>JSON per task<br/>write-tmp + fsync + rename")]
        Adapter["E2B adapter"]
    end
    E2B["E2B control plane<br/>api.e2b.app"]
    SBX["E2B sandbox<br/>code-interpreter :49999"]
    Human["Human approver"]

    Agent -->|"submit action_id"| API
    Human -->|"approve / reject / resolve"| API
    API --> Engine
    Engine <--> Store
    Engine --> Adapter
    Adapter -->|"create / connect / pause / kill"| E2B
    Adapter -->|"POST /execute (NDJSON)"| SBX
    E2B -. manages .- SBX
```

## Action lifecycle

The intent is written to disk **before** the sandbox is called. That single ordering rule is what makes
crash recovery honest.

```mermaid
stateDiagram-v2
    [*] --> pending_approval: requires_approval
    [*] --> running: submit (intent fsynced)
    pending_approval --> running: approve
    pending_approval --> rejected: reject / task cancelled
    running --> succeeded: result, no error
    running --> failed: code raised, or sandbox unavailable
    running --> failed: exec error, idempotent action
    running --> outcome_unknown: exec error / crash, non-idempotent
    running --> running: crash, idempotent, attempts left (replay)
    outcome_unknown --> succeeded: resolve(succeeded)
    outcome_unknown --> failed: resolve(failed)
    outcome_unknown --> running: resolve(retry), attempts left
    failed --> running: resolve(retry), attempts left
    succeeded --> [*]
    rejected --> [*]
```

A task can only be completed when no action is `running`, `pending_approval` or `outcome_unknown`.

## Task lifecycle

```mermaid
stateDiagram-v2
    [*] --> queued
    queued --> running: first action
    running --> waiting_for_approval: an action needs approval
    waiting_for_approval --> running: approved / rejected
    running --> succeeded: complete + evidence
    running --> failed: complete
    queued --> cancelled: cancel
    running --> cancelled: cancel
    waiting_for_approval --> cancelled: cancel
```

## Crash recovery

```mermaid
sequenceDiagram
    autonumber
    participant A as Agent
    participant T as tetherd
    participant D as Disk
    participant S as E2B sandbox

    A->>T: POST action {action_id: send-invoice}
    T->>D: save action = running (fsync)
    T->>S: execute
    Note over T,S: process killed (or response lost)
    S--xT: no result

    Note over T: restart
    T->>D: Recover(): find actions still "running"
    alt idempotent and attempts left
        T->>S: replay in background
        T->>D: record result
    else non-idempotent
        T->>D: mark outcome_unknown
    end

    A->>T: POST same action_id (agent retries)
    T-->>A: recorded state, Idempotent-Replay: true (never re-executes)
    A->>T: resolve {succeeded|failed|retry, note}
    T->>D: record reconciliation
```

## Where Pi fits

tether does not replace the agent runtime. Pi (or any agent) decides *what* to do and calls tether to do it
safely. Pi's own session persistence recovers the conversation; tether recovers the *side effects*.
Don't let two layers retry the same action: give the agent a stable `action_id` per intended effect and
let tether be the only place that decides whether it runs again.

## Scaling and deployment

tether is a single-node service by design (one process owns the data dir). That is enough for a first
production workload: E2B does the heavy, scalable part.

```mermaid
flowchart TB
    subgraph host["One VM / container (AWS, Azure, anywhere)"]
        tetherd2["tetherd"]
        vol[("Persistent volume<br/>snapshot / backup")]
        tetherd2 --- vol
    end
    clients["Agents / workers"] --> tetherd2
    tetherd2 --> e2b["E2B cloud"]
```

When you outgrow it: put a workflow engine (e.g. Temporal) in front for durable timers and long waits, and
swap `internal/store` for Postgres behind the same `Save/Load/IDs` surface. Keep tether (or its ledger
rules) as the layer that owns action dedupe and reconciliation.

## Out of scope (on purpose)

- Running Firecracker yourself (E2B does this).
- Per-user OAuth token injection. If sandboxed code needs third-party credentials, pass short-lived scoped
  tokens per task, or add an egress proxy as a separate component.
- Multi-node HA / active-active.
