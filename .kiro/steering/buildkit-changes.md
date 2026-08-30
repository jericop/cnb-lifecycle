---
inclusion: auto
---

# BuildKit-Related Lifecycle Changes

## Overview

This is a fork of `buildpacks/lifecycle` at `jericop/cnb-lifecycle`. The
`buildkit-native-export` branch adds the pieces the lifecycle needs to run inside
BuildKit and to support pack's single builder-agnostic `buildkit` build backend
(build-then-finalize). It keeps the opt-in `-skip-chown` flag and adds an
exporter emit-mode plus a finalize library. The earlier `-pull-run-image` flag
and OCI-layout export path have been REMOVED.

## Repository Structure

- **Remote**: `github.com/jericop/cnb-lifecycle`
- **Branch `buildkit-native-export`**: `-skip-chown` + exporter emit-mode
  (`phase/emit`, `layers/`) + finalize library (`phase/finalize`). Published as
  the multi-arch image `jericop/lifecycle:buildkit-native-export-v0.1.0` (from the
  same-named git tag) and consumed by pack as a library via a `go.mod` `replace`
  pinned to that tag.

## Change 1: `-skip-chown` Flag

### Problem

BuildKit runs in an unprivileged environment where `chown` syscalls are not
permitted. The lifecycle's `Privileges()` method in the analyzer, restorer, and
exporter calls `priv.EnsureOwner()` to chown `/cache`, `/workspace`, and other
volumes. This fails with `operation not permitted` inside BuildKit. The BuildKit
LLB API does not expose uid/gid settings on cache mounts, so the caller uses
`chmod`/ownership handling outside the lifecycle instead.

### Solution

An opt-in `-skip-chown` flag that skips `priv.EnsureOwner()` when set. The
lifecycle continues to call `priv.RunAs()` (setuid/setgid) to drop privileges, so
buildpack code still runs as the CNB user.

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

- **Opt-in flag** (not auto-detect): Avoids unexpected behavior changes in
  existing deployments. Users explicitly acknowledge they are in an unprivileged
  environment.
- **Still calls `priv.RunAs()`**: Privilege drop still happens, only the chown of
  volumes is skipped. Buildpack code cannot escalate privileges.
- **Per-architecture cache isolation**: The caller (pack) is responsible for using
  separate cache directories/mounts per architecture.

## Change 2: Exporter emit-mode + finalize library

### Purpose

The single `buildkit` backend builds and pushes the app image natively in
BuildKit, then a lifecycle-owned FINALIZE step authors the CNB metadata on the
pushed image from its ACTUAL produced layers. Two lifecycle pieces support this:

- **Emit-mode (`phase/emit`, `layers/`).** Instead of writing a final
  `io.buildpacks.lifecycle.metadata`, the exporter records the ordered export
  plan + the emitted CNB labels + per-layer filesystem Source refs
  (`layers.Layer.Source` → `emit.LayerOp.Source`) into a single build label,
  `io.buildpacks.lifecycle.prepared-metadata`. Pack's in-process BuildFunc reads
  those Source refs and assembles `FROM run-image` via `llb.Copy` (including
  per-slice `IncludePatterns` for app slices). No frontend is involved — the
  `buildkit/cnbfrontend` package and `cmd/cnb-frontend` are DELETED.
- **Finalize (`phase/finalize`).** After BuildKit pushes the image, pack calls
  the finalize library (used like `phase.Rebaser`). It reads the pushed image's
  produced diffIDs + the prepared-metadata label, AUTHORS
  `io.buildpacks.lifecycle.metadata` (per-layer SHAs = produced diffIDs; RunImage
  boundary), removes the prepared-metadata label, and re-pushes config +
  manifest(+index) only — no layer changes. This authors correct metadata the
  first time (one source of truth in the lifecycle) rather than rewriting SHAs
  after the fact.

### The prepared-metadata label

`io.buildpacks.lifecycle.prepared-metadata` is builder-agnostic on purpose (it
carries the prepared plan for any build engine, e.g. a future buildah-podman
backend, and is consumed by the finalize / apply-image-metadata step). It
replaces the spike-era `io.buildpacks.buildkit.native.build-metadata` and
`io.buildpacks.native.layer-order` labels.

### Keeping the label (self-heal)

`cmd/lifecycle/cli/flags.go` provides `FlagKeepPreparedMetadataLabel()`
(`-keep-prepared-metadata-label`) so finalize can retain the prepared-metadata
label for pack's opt-in self-heal path (`pack build --fix-image-metadata` /
`pack image-metadata fix`).

## Published Artifacts

### `jericop/lifecycle:buildkit-native-export-v0.1.0` (multi-arch)

Published by `.github/workflows/publish-lifecycle.yml`. The workflow triggers on
pushes to the `buildkit-native-export` branch (moving branch tag) and on pushes
of a `buildkit-native-export-v*` git tag (immutable pinned tag). On a tag push it
publishes an image whose tag matches the git tag verbatim. It runs
`make clean && make build && make package`, uses `go run ./tools/image/main.go`
to push per-arch images, then creates and pushes a Docker manifest list.

Bundled into `jericop/ubuntu-noble-builder:buildkit-native-export` via that
builder's `builder.toml` `[lifecycle].uri`.

## Relationship to Other Repos

- **jericop/cnb-pack** (`buildkit-native-export` branch): Consumes this lifecycle
  as a library (`go.mod` `replace` pinned to `buildkit-native-export-v0.1.0`).
  Drives the in-process BuildFunc (`llb.Copy` assembly) and calls
  `finalize.Finalize` post-push.
- **jericop/ubuntu-noble-builder** (`buildkit-native-export` branch): Builder that
  bundles `jericop/lifecycle:buildkit-native-export-v0.1.0`.
- **jericop/pr-compliance-app**: CI testing that exercises builds with this
  patched lifecycle.

## Future Work

- Propose upstream lifecycle changes via the RFC process (`-skip-chown`, the
  emit-mode/finalize contract, and the prepared-metadata label are additive and
  don't change default behavior).
- Optional: rename the Go packages `phase/emit` → `phase/preparemetadata` and
  `phase/finalize` → `phase/applymetadata` and the finalize subcommand to
  `apply-image-metadata` (deferred; cosmetic/high-churn).
