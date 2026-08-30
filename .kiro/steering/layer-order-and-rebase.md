---
inclusion: manual
---

# Layer Order and Rebase Compatibility

> **STATUS — invariants current; assembly framing historical.** The layer-order
> and rebase invariants documented here are timeless and still hold. However, the
> framing around a `-export-mode layers` contract and a generated Dockerfile with
> `COPY --from=lifecycle-stage /output/layers/NN/ /` reflects an EXPLORED path that
> was not chosen. In the implemented design, pack's in-process BuildFunc assembles
> the image via `llb.Copy` of each emitted CNB layer source (one `llb.Copy` per
> layer, in order), and the lifecycle `phase/finalize` library authors
> `io.buildpacks.lifecycle.metadata` from the produced diffIDs post-push. Read the
> Dockerfile/`-export-mode layers` examples below as illustrative of the ordering
> model, not the current mechanism.

## Context

An external assembly step (in the implemented design, pack's `llb.Copy`-based
BuildFunc) stacks the lifecycle-produced layers on top of the run image. For the
resulting image to be functionally equivalent to what the lifecycle normally
produces, two invariants must hold:

1. **Layer order must be correct** — layers must be assembled in the same order the lifecycle would apply them
2. **Rebase must work identically** — `pack rebase` must be able to swap run image layers without affecting buildpack/app layers

This document explains why these invariants hold in the proposed design and what testing is needed to validate them.

## How Rebase Works (Per Platform Spec)

The Platform Interface Specification defines rebase as follows:

1. The rebaser reads the `io.buildpacks.lifecycle.metadata` label from the app image
2. It identifies run image layers as all layers up to and including the one with diff ID matching `runImage.topLayer`
3. It replaces those layers with layers from the new run image
4. It updates the metadata label (`runImage.reference`, `runImage.topLayer`)
5. All layers above the run image (launcher, buildpack layers, app layers) remain unchanged

The critical data in `io.buildpacks.lifecycle.metadata`:

```json
{
  "launcher": {"sha": "<launcher-layer-diffID>"},
  "buildpacks": [
    {
      "key": "paketo-buildpacks/go-dist",
      "version": "2.5.1",
      "layers": {
        "go": {"sha": "<layer-diffID>", "launch": true, "cache": true}
      }
    }
  ],
  "app": [{"sha": "<app-layer-diffID>"}],
  "runImage": {
    "topLayer": "<run-image-top-layer-diffID>",
    "reference": "<run-image-reference>"
  }
}
```

The rebaser uses `runImage.topLayer` to find the boundary between "run image layers" (below) and "app layers" (above). Everything above that boundary is preserved during rebase.

## Why Layer Order Matters

In a standard OCI image, layers are filesystem diffs stacked from bottom to top:

```
┌─────────────────────────┐
│ App layer               │  ← /workspace contents
├─────────────────────────┤
│ Buildpack layer N       │  ← /layers/bp-N/layer-name/
├─────────────────────────┤
│ Buildpack layer 1       │  ← /layers/bp-1/layer-name/
├─────────────────────────┤
│ Launcher layer          │  ← /cnb/lifecycle/launcher
├─────────────────────────┤
│ Run image layers        │  ← OS, runtime libs, etc.
└─────────────────────────┘
```

For CNB images specifically, **each buildpack layer writes to its own isolated path** (`/layers/<buildpack-id>/<layer-name>/`). This means buildpack layers do NOT have filesystem overlap — layer B doesn't create files that depend on layer A existing below it. They're independent.

However, the order still matters because:
1. The `io.buildpacks.lifecycle.metadata` label records the diff ID of each layer. If layers are reordered, the diff IDs change (because a layer diff is relative to the layers below it), and the metadata would be inconsistent.
2. The rebaser identifies the run image boundary using `topLayer`. If buildpack layers appear below the run image's top layer, rebase would incorrectly discard them.
3. Reproducibility — the image digest must be deterministic. Same build = same layer order = same digest.

## How Our Design Preserves Order

The manifest.json `layers` array is an **ordered list**. The assembling tool (BuildKit/Buildah) MUST apply layers in the order specified:

```json
{
  "layers": [
    {"path": "layers/00-launcher/", "metadata": {"type": "launcher"}},
    {"path": "layers/01-go-dist/", "metadata": {"type": "buildpack", "buildpack_id": "paketo-buildpacks/go-dist"}},
    {"path": "layers/02-go-build/", "metadata": {"type": "buildpack", "buildpack_id": "paketo-buildpacks/go-build"}},
    {"path": "layers/03-app/", "metadata": {"type": "app"}}
  ]
}
```

The generated Dockerfile applies them in this exact order:

```dockerfile
FROM <run-image>
COPY --from=lifecycle-stage /output/layers/00-launcher/ /
COPY --from=lifecycle-stage /output/layers/01-go-dist/ /
COPY --from=lifecycle-stage /output/layers/02-go-build/ /
COPY --from=lifecycle-stage /output/layers/03-app/ /
```

Each COPY becomes one OCI layer in the final image, applied in the order written. The resulting layer stack is:

```
Run image layers (from FROM) → launcher → buildpack layers in order → app
```

This matches exactly what the lifecycle produces when it exports directly to a registry.

## How Our Design Preserves Rebase

For rebase to work, the resulting image must have:

1. **`io.buildpacks.lifecycle.metadata` label** with correct diff IDs for each layer and the run image's `topLayer`
2. **Run image layers at the bottom** (from the `FROM <run-image>` instruction)
3. **Buildpack/app layers above** the run image, each with a stable diff ID that matches what's recorded in the metadata label

Our design satisfies all three:

1. The lifecycle computes the metadata label as part of the export process (it already does this today in registry/daemon mode). The same label content is included in `manifest.json` → `image_config.labels`. The assembling tool applies it to the final image.

2. The `FROM <run-image>` instruction means run image layers are at the bottom, exactly as if the lifecycle had built the image starting from the run image (which it does today).

3. Each buildpack layer is written to its own directory (`/output/layers/NN-name/`). When the assembling tool does `COPY ... /output/layers/01-go-dist/ /`, it produces a layer diff containing exactly the files the lifecycle would have included in that layer. The diff ID is the sha256 of the uncompressed tar of that diff — deterministic for the same content.

**Critical requirement**: The diff IDs recorded in the metadata label MUST match the diff IDs of the layers in the assembled image. Since the lifecycle computes the diff IDs from the same layer content that it writes to disk, and the assembling tool creates layers from that same content, they WILL match — provided the tool applies them in the correct order on top of the same run image.

## Potential Risks

### Risk 1: Layer diff computation differences

If BuildKit computes a layer diff differently than what the lifecycle recorded (e.g., different tar serialization, different file ordering in the tar), the diff IDs won't match the metadata label.

**Mitigation**: The lifecycle should pre-compute diff IDs based on the actual file content and include them in manifest.json. The assembling tool should verify its computed diff IDs match. If they don't, the image is still functional but `pack rebase` may not identify layers correctly.

**Testing needed**: Compare diff IDs from a lifecycle registry push vs. the same build assembled via BuildKit.

### Risk 2: Run image layer identification

The lifecycle records `runImage.topLayer` as the diff ID of the run image's top layer. When the assembling tool does `FROM <run-image>`, it pulls that image's layers. The top layer's diff ID must match what the lifecycle recorded in the metadata.

**Mitigation**: The lifecycle already resolves the run image and computes `topLayer` during the analyze phase. The same run image reference is passed to both the lifecycle and the assembling tool's `FROM` instruction.

### Risk 3: Multi-platform rebase

In multi-platform builds, each architecture has its own image with its own layers. Rebase operates on individual platform images, not the manifest list. Our design handles this naturally since each platform produces its own manifest.json with platform-specific layer content and metadata.

## Testing Plan

### Test 1: Layer order parity

Build the same app two ways:
1. Standard `pack build` (lifecycle pushes to registry directly)
2. BuildKit-assembled build (lifecycle exports layers → BuildKit assembles)

Compare: the `docker manifest inspect` output should show the same number of layers in the same order.

### Test 2: Diff ID parity

For both builds above, compare the diff IDs in the `io.buildpacks.lifecycle.metadata` label. They should be identical.

If they differ, investigate whether the difference is in tar serialization (file order, timestamps, permissions) between lifecycle's direct push and BuildKit's layer assembly.

### Test 3: Rebase after standard build

1. Standard `pack build` → app image
2. `pack rebase` with a new run image
3. Verify: run image layers replaced, buildpack/app layers preserved, app works

### Test 4: Rebase after BuildKit-assembled build

1. BuildKit-assembled build → app image
2. `pack rebase` with the same new run image as Test 3
3. Verify: same behavior as Test 3 — run image layers replaced, buildpack/app layers preserved, app works

### Test 5: Cross-rebase (build one way, rebase the other)

1. BuildKit-assembled build → app image
2. Standard `pack rebase` (not BuildKit) → should work identically
3. Standard `pack build` → app image
4. Rebase should work the same regardless of how the image was originally built

### Test 6: Multi-arch rebase

1. BuildKit multi-arch build (amd64 + arm64) → manifest list
2. `pack rebase` with new multi-arch run image
3. Verify: both platform images rebased correctly, manifest list updated

## Spec Requirements to Factor In

From the Platform Interface Specification:

1. **Layer metadata label is mandatory**: The exporter MUST set `io.buildpacks.lifecycle.metadata` with correct diff IDs for all layers. Our design includes this in manifest.json → image_config.labels, and the assembling tool MUST apply it.

2. **Run image topLayer must be accurate**: `runImage.topLayer` MUST contain the uncompressed digest of the top layer of the run image. Since the assembling tool uses `FROM <run-image>`, this is automatically correct — the run image's layers come from the same image the lifecycle analyzed.

3. **Rebaser identifies boundary by topLayer**: The rebaser finds all layers up to and including the one matching `topLayer` and replaces them. Our design preserves this because run image layers are at the bottom (from `FROM`) and all lifecycle-produced layers are above.

4. **Layer diff IDs must match metadata**: Each buildpack layer's `sha` in the metadata must match the actual diff ID in the image. This is the critical parity requirement — our tests must verify this.

5. **App image must be rebasable**: The label `io.buildpacks.rebasable` must be set correctly. The lifecycle already handles this; it's included in the metadata label output.

6. **Spec says "replace run image layers"**: The spec defines run image layers as everything up to and including `topLayer`. As long as our assembled image has run image layers at the bottom (from `FROM <run-image>`) followed by lifecycle layers above, rebase will correctly identify the boundary.

## Summary

The proposed design maintains full rebase compatibility because:
- Layer order is explicit in manifest.json (ordered array) and enforced by numbered directory names
- The assembling tool produces the same layer stack as a direct lifecycle export
- The `io.buildpacks.lifecycle.metadata` label contains the same diff IDs
- The run image is at the bottom (from `FROM`) exactly as in today's images
- Each buildpack layer is independent and self-contained at its own path

The main validation needed is **diff ID parity**: confirming that BuildKit/Buildah produce the same layer diff IDs as the lifecycle would compute when pushing directly. If tar serialization differs, we may need the lifecycle to write pre-computed tarballs (Option A in the contract doc) rather than expanded directories, or accept that diff IDs will differ and adjust the metadata accordingly.
