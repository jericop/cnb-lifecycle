---
inclusion: auto
---

# BuildKit Lifecycle Flags: -skip-chown and -pull-run-image

## Purpose

This document explains why the lifecycle needs two new opt-in flags to operate correctly inside BuildKit environments. These flags are additive — they don't change default behavior and are only activated when explicitly passed. They are prerequisites for the BuildKit multi-architecture build feature being developed in pack.

Both flags will be proposed upstream via PR to buildpacks/lifecycle. This document serves as the rationale for those changes.

---

## Flag 1: `-skip-chown`

### Problem

BuildKit executes `RUN` instructions in an unprivileged container environment. The `chown` syscall is not permitted — it fails with `EPERM (operation not permitted)`.

The lifecycle's analyzer, restorer, and exporter call `priv.EnsureOwner()` to chown volumes (`/cache`, `/workspace`, `/layers`) to the CNB user before dropping privileges. This call fails unconditionally inside BuildKit:

```
Error: ensuring ownership: chown /cache: operation not permitted
```

This happens regardless of whether the process is running as root inside the container, because BuildKit's rootless execution model restricts `chown` at the kernel level (via user namespaces or seccomp).

### Why this only affects BuildKit

In pack's traditional flow, lifecycle phases run as individual Docker containers with full root capabilities. The `chown` succeeds because the Docker daemon grants CAP_CHOWN to the container. BuildKit's execution model is fundamentally different — RUN instructions execute in a sandboxed environment where many syscalls (including chown) are restricted.

For the **Dockerfile backend**, the Dockerfile frontend handles cache mount ownership internally via an `[internal] setting cache mount permissions` step that uses kernel-level mount options — not userspace chown. This is why `--mount=type=cache,uid=1001,gid=1001` works in a Dockerfile even though chown would fail. But the lifecycle doesn't know this has already been handled and attempts chown anyway.

For the **LLB backend**, the BuildKit LLB API doesn't expose uid/gid settings on cache mounts at all (that's a Dockerfile frontend-specific capability). So the LLB backend uses `chmod 777` on cache directories as a setup step, making them writable by the CNB user without chown.

### Solution

An opt-in `-skip-chown` flag that tells the lifecycle to skip `priv.EnsureOwner()`. The lifecycle continues to call `priv.RunAs()` (setuid/setgid) to drop privileges, so buildpack code still runs as the CNB user. The only difference is that volume ownership is not changed — the caller (pack/BuildKit) is responsible for ensuring volumes are writable by the CNB user before the lifecycle runs.

### Why opt-in (not auto-detect)

We considered auto-detecting the unprivileged environment (try chown, catch EPERM, continue), but this was rejected because:

1. **Explicit is better than implicit** — Users/platforms should consciously acknowledge they're in an unprivileged environment
2. **Fail-fast for real permission issues** — If chown fails unexpectedly in a traditional Docker environment, it should surface as an error rather than being silently swallowed
3. **Testability** — The behavior is deterministic based on the flag value, not the runtime environment
4. **Spec compliance** — The platform spec doesn't mandate chown; it's an implementation detail. Making it skippable doesn't violate any spec requirement.

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

- `priv.RunAs()` still executes — privileges are dropped to the CNB user
- Buildpack code still runs as UID 1001 (or whatever the builder specifies)
- The only difference is volume ownership: the platform guarantees writability instead of the lifecycle
- No new attack surface is introduced

---

## Flag 2: `-pull-run-image`

### Problem

When the lifecycle runs in OCI layout mode (`-layout -layout-dir /output`), the analyzer expects the run image to already exist in the layout directory. In pack's traditional flow, pack pre-populates this by pulling the run image and writing it to the layout directory before invoking the lifecycle.

Inside BuildKit, pack cannot pre-populate the layout directory because:
1. The filesystem is managed by BuildKit — pack has no direct access to it
2. The layout directory exists only inside the build container
3. There's no mechanism to inject content into a BuildKit RUN instruction's filesystem from outside

This means the lifecycle's layout mode is unusable inside BuildKit without a way to self-serve the run image.

### Why this only affects BuildKit (and similar environments)

In pack's traditional Docker-container flow:
1. Pack pulls the run image
2. Pack writes it to the layout directory (mounted as a volume)
3. Pack invokes the lifecycle with the layout directory pre-populated

In BuildKit:
1. The lifecycle runs inside a `RUN` instruction
2. No external process can write to the container's filesystem during execution
3. The layout directory starts empty
4. The lifecycle fails: "run image not found in layout directory"

The same problem would occur in any environment where the lifecycle is invoked without the platform being able to pre-populate the layout directory (e.g., Buildah, Podman build, Kubernetes Jobs).

### Solution

An opt-in `-pull-run-image` flag on the analyzer that pulls the run image from a registry into the layout directory before analysis begins. The lifecycle already has registry access (via `CNB_REGISTRY_AUTH` or the Docker keychain) and knows the run image reference — it just needs permission to pull it itself.

### Implementation

A new `PullToLayout()` function:
1. Parses the run image reference using `go-containerregistry`
2. Computes the layout path using `imgutil/layout.ParseRefToPath()`
3. Checks if already present (idempotent — skips if exists)
4. Pulls from registry using the lifecycle's keychain (respects `CNB_REGISTRY_AUTH`)
5. Writes to the layout directory using `imgutil/layout.Write()`

### Why opt-in (not default behavior)

1. **Backward compatibility** — Existing platforms that pre-populate the layout directory shouldn't have behavior change
2. **Network isolation** — Some environments deliberately restrict network access during the build. The lifecycle shouldn't unexpectedly make network calls.
3. **Performance** — If the platform has already pulled the run image, pulling it again is wasteful
4. **Spec alignment** — The platform spec says the platform provides the run image. This flag makes the lifecycle optionally self-sufficient, but the default still expects the platform to provide it.

### Files changed

| File | Change |
|------|--------|
| `cmd/lifecycle/cli/flags.go` | Added `FlagPullRunImage()` |
| `platform/lifecycle_inputs.go` | Added `PullRunImage bool` field |
| `cmd/lifecycle/analyzer.go` | Added pull logic before `factory.NewAnalyzer()` |
| `image/pull_to_layout.go` | New file: `PullToLayout()` function |

### Usage

```bash
/cnb/lifecycle/analyzer -pull-run-image -layout -layout-dir /output \
  -skip-chown -uid 1001 -gid 1001 -run-image <run-image> <image-tag>
```

### When both flags are used together

In the BuildKit multi-arch build flow, both flags are passed together:

```bash
# Inside a BuildKit RUN instruction:
/cnb/lifecycle/analyzer \
  -skip-chown \           # BuildKit can't chown
  -pull-run-image \       # BuildKit can't pre-populate layout dir
  -layout -layout-dir /output \
  -uid 1001 -gid 1001 \
  -run-image paketobuildpacks/run-jammy-tiny:latest \
  myapp:latest
```

The lifecycle then:
1. Skips chown (volumes are pre-configured writable by pack's Dockerfile setup)
2. Pulls the run image into `/output/` (self-service, since pack can't do it)
3. Proceeds with normal analysis using the layout directory

---

## Relationship to `-export-mode layers` (Future)

Both flags are prerequisites for the `-export-mode layers` feature:

- `-skip-chown` is needed because the lifecycle will run inside BuildKit where chown fails
- `-pull-run-image` is needed because the lifecycle needs the run image to compute metadata (topLayer, diff IDs) and the platform can't pre-populate it inside BuildKit

The `-export-mode layers` feature adds a third dimension: instead of pushing the final image to a registry, the lifecycle writes decomposed layers to disk for the build tool to assemble. But it still needs the run image information (for the metadata label) and still can't chown (because it's in BuildKit).

## Upstream PR Considerations

When proposing these changes to buildpacks/lifecycle:

1. **Both flags are additive** — No existing behavior changes. All existing tests pass without modification.
2. **Both flags are opt-in** — Platforms that don't use BuildKit never pass these flags.
3. **No spec changes required** — The Platform Interface Spec doesn't mandate chown behavior or that the platform must pre-populate the layout directory (it says the platform "provides" the run image, which this flag satisfies by the lifecycle doing it itself).
4. **Minimal code change** — `-skip-chown` is 3 `if` statements wrapping existing `EnsureOwner` calls. `-pull-run-image` is one new function that's called conditionally.
5. **Use cases beyond BuildKit** — Any unprivileged execution environment benefits from `-skip-chown` (Kubernetes Jobs without root, rootless Podman, etc.). `-pull-run-image` benefits any environment where the layout directory can't be pre-populated externally.

## Published Artifacts

- **Branch `skip-chown`**: Contains `-skip-chown` flag implementation
- **Branch `buildkit-multi-arch-support`**: Contains `-pull-run-image` on top of `skip-chown`
- **Image `jericop/lifecycle:skip-chown-poc`**: Multi-arch lifecycle with `-skip-chown` support
- **Builder `jericop/ubuntu-noble-builder:skip-chown-poc`**: Builder embedding the patched lifecycle
