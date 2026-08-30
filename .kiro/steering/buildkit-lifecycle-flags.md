---
inclusion: auto
---

# BuildKit Lifecycle Flag: -skip-chown

## Purpose

This document explains why the lifecycle needs the opt-in `-skip-chown` flag to
operate correctly inside BuildKit environments. The flag is additive — it doesn't
change default behavior and is only activated when explicitly passed. It is a
prerequisite for the BuildKit multi-architecture build feature developed in pack.

It will be proposed upstream via PR to buildpacks/lifecycle. This document serves
as the rationale for that change.

> An earlier spike also added a `-pull-run-image` flag (for an OCI-layout export
> mode). Both that flag and the OCI-layout mode have been REMOVED; the current
> design pushes natively in BuildKit and authors CNB metadata in a post-push
> finalize step (see `buildkit-changes.md`). Only `-skip-chown` remains.

---

## `-skip-chown`

### Problem

BuildKit executes `RUN` instructions in an unprivileged container environment. The
`chown` syscall is not permitted — it fails with `EPERM (operation not permitted)`.

The lifecycle's analyzer, restorer, and exporter call `priv.EnsureOwner()` to
chown volumes (`/cache`, `/workspace`, `/layers`) to the CNB user before dropping
privileges. This call fails unconditionally inside BuildKit:

```
Error: ensuring ownership: chown /cache: operation not permitted
```

This happens regardless of whether the process is running as root inside the
container, because BuildKit's rootless execution model restricts `chown` at the
kernel level (via user namespaces or seccomp).

### Why this only affects BuildKit

In pack's traditional flow, lifecycle phases run as individual Docker containers
with full root capabilities, so `chown` succeeds. BuildKit's execution model is
different — RUN instructions execute in a sandboxed environment where many
syscalls (including chown) are restricted. The BuildKit LLB API does not expose
uid/gid settings on cache mounts, so the caller ensures cache directories are
writable by the CNB user outside the lifecycle instead.

### Solution

An opt-in `-skip-chown` flag that tells the lifecycle to skip `priv.EnsureOwner()`.
The lifecycle continues to call `priv.RunAs()` (setuid/setgid) to drop privileges,
so buildpack code still runs as the CNB user. The only difference is that volume
ownership is not changed — the caller (pack/BuildKit) is responsible for ensuring
volumes are writable by the CNB user before the lifecycle runs.

### Why opt-in (not auto-detect)

1. **Explicit is better than implicit** — Users/platforms should consciously
   acknowledge they're in an unprivileged environment.
2. **Fail-fast for real permission issues** — If chown fails unexpectedly in a
   traditional Docker environment, it should surface as an error rather than being
   silently swallowed.
3. **Testability** — Behavior is deterministic based on the flag value, not the
   runtime environment.
4. **Spec compliance** — The platform spec doesn't mandate chown; it's an
   implementation detail. Making it skippable doesn't violate any spec requirement.

### Files changed

| File | Change |
|------|--------|
| `cmd/lifecycle/cli/flags.go` | Added `FlagSkipChown()` |
| `platform/lifecycle_inputs.go` | Added `SkipChown bool` field |
| `cmd/lifecycle/analyzer.go` | Wrapped `priv.EnsureOwner()` in `if !a.SkipChown` |
| `cmd/lifecycle/restorer.go` | Wrapped `priv.EnsureOwner()` in `if !r.SkipChown` |
| `cmd/lifecycle/exporter.go` | Wrapped `priv.EnsureOwner()` in `if !e.SkipChown` |

### Usage

```bash
/cnb/lifecycle/analyzer -skip-chown -uid 1001 -gid 1001 -run-image <run-image> <image-tag>
/cnb/lifecycle/restorer -skip-chown -uid 1001 -gid 1001 -cache-dir /cache
/cnb/lifecycle/exporter -skip-chown -uid 1001 -gid 1001 -cache-dir /cache <image-tag>
```

### Security considerations

- `priv.RunAs()` still executes — privileges are dropped to the CNB user.
- Buildpack code still runs as UID 1001 (or whatever the builder specifies).
- The only difference is volume ownership: the platform guarantees writability
  instead of the lifecycle.
- No new attack surface is introduced.

---

## Upstream PR Considerations

When proposing this change to buildpacks/lifecycle:

1. **Additive** — No existing behavior changes. All existing tests pass without
   modification.
2. **Opt-in** — Platforms that don't use BuildKit never pass the flag.
3. **No spec changes required** — The Platform Interface Spec doesn't mandate
   chown behavior.
4. **Minimal code change** — `-skip-chown` is 3 `if` statements wrapping existing
   `EnsureOwner` calls.
5. **Use cases beyond BuildKit** — Any unprivileged execution environment benefits
   (Kubernetes Jobs without root, rootless Podman, etc.).

## Published Artifacts

- **Branch `buildkit-native-export`**: Contains `-skip-chown` plus the exporter
  emit-mode + finalize library (see `buildkit-changes.md`).
- **Image `jericop/lifecycle:buildkit-native-export-v0.1.0`**: Multi-arch
  lifecycle published from the same-named git tag.
- **Builder `jericop/ubuntu-noble-builder:buildkit-native-export`**: Builder
  embedding that pinned lifecycle.
