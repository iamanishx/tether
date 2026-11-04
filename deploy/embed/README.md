# Vendored E2B Embed recipe

`compose.yaml` and the secret-free `pins.env` were retrieved from the official
[e2b-dev/runtime Embed Compose distribution](https://github.com/e2b-dev/runtime/tree/main/embed/compose)
on 2026-11-04. They are third-party Apache-2.0 material; see `LICENSE.upstream`.

The image and runtime binary versions are pinned in `pins.env`. The `RUNTIME_COMMIT` value belongs to
upstream's published pins; it is not a claim that this downloaded Compose file was verified against
that exact Git commit. Updating this recipe requires reviewing both files together and re-running live
compatibility checks.

`scripts/local.py` resolves the recipe, generates an ignored local configuration and makes these
explicit development adaptations:

- removes the dashboard/optional telemetry collector;
- assigns owned volume/network names;
- defaults to a 512 MiB guest hugepage pool and modest service limits;
- disables PostgreSQL's competing hugepage allocation;
- reserves the VM pool before databases start;
- protects internal host-network ports with a loopback-only firewall rule;
- discovers the generated E2B team key privately and launches the Tether container;
- admits one action at a time and requests guest pause after each action.

E2B's preflight/orchestrator/host helpers require privileged host access. Agents execute in real
Firecracker guests, not in those privileged platform containers. The recipe modifies host kernel
modules, NBD, hugepages, sysctls, directories and networking. `local-down` does not undo this setup;
`local-purge` is the explicit teardown path.

This packaging reached readiness locally, but the complete live test still fails during a paused
guest's capacity-constrained reconnect. See [the test status](../../docs/TESTING.md). Do not expose
this local development configuration as a public multi-tenant production service.
