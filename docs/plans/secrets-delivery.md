# Secrets Delivery Plan — croft

Give workspaces a secrets story that does not leak through `/proc`: **secret
mounts** (host files bind-mounted read-only into the container — an agent's
git token arrives as a file, never in argv, env, or a gRPC payload) and an
**argv-free write path** (`Config.StagingDir` — when set, `WriteFile` copies
host-side and moves into place inside the container, closing the base64-in-
argv exposure for every write, and removing the base64 inflation for large
files as a side effect). Ships as croft v0.7.0; the broker maps tenant
credentials onto both.

## Part 0 — Baseline and scope boundaries

Assumes v0.4.0 (lean) has landed; touches `initSpecOpts` and `WriteFile`,
independent of the diff/delta/exec-I/O phases.

**The exposure inventory this plan answers** (stated in the README today):

| Surface | Exposure | After this plan |
|---|---|---|
| `WriteFile` | base64 content in argv → host-readable `/proc/*/cmdline` | closed when `StagingDir` is set; unchanged otherwise (documented) |
| `ExecRequest.Stdin` (phase 4) | same argv staging path as WriteFile | closed for writes; stdin content itself still stages via the argv path — documented |
| `ExecRequest.Env` | `/proc/*/environ` of the exec'd process | **not addressed** — env is not a secrets channel; documented as such |
| `Exec` args | argv by definition | **not addressed** — a caller passing a secret as an argument is a caller bug; documented |
| Secret *delivery* | previously: none — hosts smuggled tokens via WriteFile or env | **solved**: bind-mounted files |

**Decided scope boundary (2026-10-13):** secrets are files, not a vault.
Croft does not encrypt, rotate, issue, or audit credentials — it delivers
files the caller already controls into the boundary without transiting any
leaking surface. Vault integration is the broker's or host's concern.

## Part 1 — Mechanism

### 1.1 Secret mounts

```go
type Config struct {
    // ...
    // Secrets are host files bind-mounted read-only into the container.
    // The container reads a normal file; the content never appears in
    // argv, process environment, or any exec payload.
    Secrets []SecretMount
}

type SecretMount struct {
    // Source is an absolute path on the daemon host. Must exist and be
    // readable by the daemon's user; protect it there (0600) — croft
    // validates existence, not permission posture.
    Source string
    // Destination is the absolute in-container path (e.g.
    // /run/secrets/git-token).
    Destination string
}
```

In `initSpecOpts`, each becomes a bind mount on the spec
(`spec.Mounts{Type: "bind", Source, Destination, Options: ["ro"]}`) — the
same mechanism Kubernetes uses for secret files, with the same property: the
secret is filesystem data, and nothing about its delivery is observable from
outside the container beyond the host file itself.

Constraints stated in the doc comment and README:

- **Host coupling**: the source path must exist on the daemon's host — the
  same coupling `FIFODir` already has; remote clients stage their secrets
  through whatever shared path they already have. The broker (on the daemon
  host) owns a 0600 per-tenant directory and maps tenant credentials into
  mounts — the broker's own process never sees the material in argv either.
- **Ownership**: a bind mount preserves the host file's uid/gid/mode. With
  the default (no uid remapping), container root reads a host-root 0600
  file; a remapped deployment is a documented boundary, not silently fixed.
- **Destination validation**: absolute, no overlap with `Workdir`'s root,
  rejected loudly otherwise.

### 1.2 Argv-free writes via a staging mount

`WriteFile` today shells out with base64 chunks in argv. With
`Config.StagingDir` set (a host directory), the flow becomes:

1. The client writes the payload to a fresh file **on the host**, inside the
   staging directory (plain file write, no exec at all).
2. The staging directory is bind-mounted read-write into the container at a
   fixed path (`/run/croft-staging`).
3. One exec moves the file into place **atomically**: copy to a temp name in
   the destination's directory, then `rename(2)` — `mv` across the staging
   mount boundary would be copy+unlink (EXDEV, non-atomic), so the move is
   `cat < staging > target.new && mv target.new target` inside the
   container's destination directory. The content appears in no argv at any
   point.

Unset `StagingDir` keeps today's argv path (remote clients with no shared
filesystem); the README states both postures. Size caps and mode handling
are unchanged.

### 1.3 What the fake and live suites prove

- Fake: the spec's mounts carry the configured secrets verbatim; a staging
  write records **no** exec whose argv contains base64 content (the fake's
  recorded request log is the assertion surface).
- Live: `TestLive_SecretMountReadable` (the file reads back exactly, mode
  included), `TestLive_WriteViaStagingRoundTrip`, and a residue check —
  no `croft-staging` leftovers after writes complete.

## Part 2 — Tasks (one task per commit, bisectable)

### Task 1 — Secret mounts
- `SecretMount` + `Config.Secrets`; destination validation; the spec mounts
  in `initSpecOpts`; fake spec assertions + validation tests.

### Task 2 — Staging writes
- `Config.StagingDir`; the host-side write; the bind mount; the atomic
  in-container move; failure paths (staging write fails → no exec; the move
  fails → host staging file removed cancel-free, the house rule).
- Fake: no base64 argv recorded; live: the round-trip and residue tests.

### Task 3 — Broker mapping
- The broker stores tenant credential material as 0600 files in its own
  per-tenant directory and passes `Secrets` mounts per sandbox — never env,
  never argv. Its `create_sandbox` grows no secret argument; secret
  assignment is tenant-admin config, not a per-call capability.

### Task 4 — Docs + CHANGELOG v0.7.0
- README contracts: the exposure table (post-plan), the host-coupling
  caveat, "env is not a secrets channel," and the vault boundary.
- `docs/overview.md` configuration section: `Secrets`, `StagingDir`.
- CHANGELOG: **Added** `Config.Secrets`, `Config.StagingDir`; **Changed**
  (documented) WriteFile's exposure posture when staging is configured.

## Risks

- **Host-path coupling** — the same model `FIFODir` already establishes;
  stated, not hidden. Remote-client deployments document their staging
  share or accept argv writes.
- **Mount-destination collisions** — validated loudly; the workdir root is
  protected specifically because agents write there.
- **EXDEV atomicity** — handled by the copy-then-rename move; the test
  asserts the temp file is gone on both success and failure paths.
- **Not a vault** — rotation, issuance, and audit remain outside; the README
  says so in one sentence.

## Validation

1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green;
   `go test ./... && go test -race ./...` green.
2. Live suite including the three new tests against a real daemon.
3. Exposure check: the fake's recorded exec log for a staging write contains
   no base64 payload — the assertion that closes the gap.

## Out of scope

- Vault/KMS integration, rotation, issuance, audit (host/broker concerns).
- Env-based secret delivery (rejected: `/proc/*/environ`).
- Encrypting the staging directory at rest.
- Streaming writes (whole-file buffers remain the scale envelope).
