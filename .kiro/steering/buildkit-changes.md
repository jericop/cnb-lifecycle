---
inclusion: auto
---

# BuildKit-Related Lifecycle Changes

## Overview

This is a fork of `buildpacks/lifecycle` at `jericop/cnb-lifecycle` with two feature branches adding opt-in flags that enable the lifecycle to run inside BuildKit environments. These changes support the BuildKit multi-architecture build feature being developed in `jericop/pack`.

## Repository Structure

- **Remote**: `github.com/jericop/cnb-lifecycle`
- **Branch `skip-chown`**: Adds `-skip-chown` flag (published as `jericop/lifecycle:skip-chown-poc`)
- **Branch `buildkit-multi-arch-support`**: Adds `-pull-run-image` flag on top of `skip-chown` (tag TBD)

## Change 1: `-skip-chown` Flag

### Problem

BuildKit runs in an unprivileged environment where `chown` syscalls are not permitted. The lifecycle's `Privileges()` method in the analyzer, restorer, and exporter calls `priv.EnsureOwner()` to chown `/cache`, `/workspace`, and other volumes. This fails with `operation not permitted` inside BuildKit.

For the **Dockerfile backend**, the Dockerfile frontend handles `uid`/`gid` on `--mount=type=cache` internally, so the cache mount is already owned by the correct user. For the **LLB backend**, the BuildKit LLB API does not expose uid/gid settings on cache mounts, so `chmod 777` is used on cache directories instead.

### Solution

An opt-in `-skip-chown` flag that skips `priv.EnsureOwner()` when set. The lifecycle continues to call `priv.RunAs()` (setuid/setgid) to drop privileges, so buildpack code still runs as the CNB user.

### Files Changed

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

### Design Decisions

- **Opt-in flag** (not auto-detect): Avoids unexpected behavior changes in existing deployments. Users explicitly acknowledge they are in an unprivileged environment.
- **Still calls `priv.RunAs()`**: Privilege drop still happens, only the chown of volumes is skipped. Buildpack code cannot escalate privileges.
- **Per-architecture cache isolation**: The caller (pack) is responsible for using separate cache directories/mounts per architecture (via `${TARGETARCH}` in cache mount IDs).

## Change 2: `-pull-run-image` Flag

### Problem

When the lifecycle runs in OCI layout mode (`-layout -layout-dir /layout`), it expects the run image to already exist in the layout directory. In pack's normal flow, pack pre-populates this. Inside BuildKit, pack cannot pre-populate the layout directory because the filesystem is managed by BuildKit.

### Solution

An opt-in `-pull-run-image` flag on the analyzer that pulls the run image from a registry into the layout directory before analysis begins.

### Files Changed

| File | Change |
|------|--------|
| `cmd/lifecycle/cli/flags.go` | Added `FlagPullRunImage()` |
| `platform/lifecycle_inputs.go` | Added `PullRunImage bool` field |
| `cmd/lifecycle/analyzer.go` | Added pull logic before `factory.NewAnalyzer()` |
| `image/pull_to_layout.go` | New file: `PullToLayout()` function |

### How `PullToLayout()` Works

1. Parses the image reference using `go-containerregistry`
2. Computes the layout path using `imgutil/layout.ParseRefToPath()`
3. Skips if already present (idempotent)
4. Pulls from registry using the lifecycle's keychain (respects `CNB_REGISTRY_AUTH`)
5. Writes to the layout directory using `imgutil/layout.Write()`

### Usage

```bash
/cnb/lifecycle/analyzer -pull-run-image -layout -layout-dir /layout \
  -skip-chown -uid 1001 -gid 1001 -run-image <run-image> <image-tag>
```

### Status

This flag is implemented but not yet tested end-to-end in the CI workflow. The OCI layout export mode in pack (`--buildkit-export-mode oci-layout`) is not yet functional because it depends on this flag.

## Published Artifacts

### `jericop/lifecycle:skip-chown-poc` (multi-arch)

Published by `.github/workflows/publish-lifecycle.yml` on pushes to the `skip-chown` branch. Builds both `linux/amd64` and `linux/arm64` lifecycle binaries and creates a manifest list.

Used by `jericop/ubuntu-noble-builder:skip-chown-poc` which embeds this lifecycle.

### Publish Workflow

The workflow:
1. Runs `make clean && make build && make package` to produce tarballs
2. Uses `go run ./tools/image/main.go` to push per-arch images
3. Creates and pushes a Docker manifest list

## Relationship to Other Repos

- **jericop/pack** (`buildkit-multi-arc-poc` branch): Consumes this lifecycle via the builder image. Passes `-skip-chown` in generated Dockerfiles and LLB graphs.
- **jericop/ubuntu-noble-builder** (`skip-chown-lifecycle` branch): Builder that bundles `jericop/lifecycle:skip-chown-poc`.
- **jericop/pr-compliance-app** (`pack-buildkit-poc` branch): CI testing that exercises both the Dockerfile and LLB backends with this patched lifecycle.

## Future Work

- Publish a `jericop/lifecycle:buildkit-multi-arch-support` tag from the `buildkit-multi-arch-support` branch
- Test `-pull-run-image` end-to-end with pack's OCI layout export mode
- Propose upstream lifecycle changes via RFC process (these flags are additive and don't change default behavior)
