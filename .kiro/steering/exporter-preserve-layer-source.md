---
inclusion: manual
---

# Exporter: Preserve Caller-Provided Layer Source in `addOrReuseBuildpackLayer`

## Purpose

This document records a small, targeted change made to
`phase/exporter.go` as part of the BuildKit-native export work, so it can be
referenced when preparing the PR. It explains what changed, why, and why the
change is safe (forward- AND backward-compatible).

The change is a **bug fix on a pre-existing upstream function**, layered under
the fork's new BuildKit-native emit path. It is NOT a new feature and does not
need to be called out in the RFC — but the PR description should explain it so a
reviewer understands why the two lines are there.

## The function is pre-existing upstream code

`func (e *Exporter) addOrReuseBuildpackLayer(...)` is NOT something the fork's
BuildKit multi-arch work introduced. Git history confirms:

- `git log -S 'func (e *Exporter) addOrReuseBuildpackLayer' -- phase/exporter.go`
  returns only `6c7d16a7 "Move lifecycle package to sub-directory (#1205)"`, an
  upstream buildpacks/lifecycle PR that relocated the package into `phase/`. The
  function existed before that; the commit merely moved it.
- `git blame` attributes the function signature and the
  `DirLayer(layer.ID, layer.TarPath, createdBy)` re-derivation line to
  **Natalie Arellano, 2023-05-30** (upstream), and the following `if err != nil`
  to **Lukas Berger, 2019-10-21** (upstream).

So both the function and its `DirLayer(id, TarPath, ...)` re-derivation behavior
are entirely upstream. The fork only added a preservation fix on top.

## What upstream does (and the latent problem it creates for us)

`addOrReuseBuildpackLayer` re-derives the layer before add/reuse:

```go
layer, err := e.LayerFactory.DirLayer(layer.ID, layer.TarPath, createdBy)
```

It passes `layer.TarPath` as `DirLayer`'s `fromDir`. Upstream this is fine: on
this code path `writeLayer` returns the already-built (cached) tar, so the actual
tar bytes / digest / history are correct and that is all upstream consumes.

The fork added a new nullable field, `layers.Layer.Source *LayerSource`, that the
caller populates with the REAL filesystem source directory the layer was built
from (see `layers/dir.go`, `layers/launcher.go`, `layers/slices.go`). The
BuildKit-native consumer uses `Source` to assemble the image with a native copy
(`llb.Copy` from the built path) instead of extracting the tar.

The problem: the upstream `DirLayer(layer.ID, layer.TarPath, ...)` re-derivation
returns a fresh `Layer` whose `Source.Dir` is stamped as the **tar path** (the
`fromDir` argument), which is a bogus source directory. Left as-is, this would
overwrite the correct caller-provided `Source` and the consumer would try to copy
from a tar path instead of the real layer dir.

## The fix (two lines around the upstream re-derivation)

```go
func (e *Exporter) addOrReuseBuildpackLayer(image imgutil.Image, layer layers.Layer, previousSHA, createdBy string) (string, error) {
	incomingSource := layer.Source                                    // ADDED: capture caller's real Source
	layer, err := e.LayerFactory.DirLayer(layer.ID, layer.TarPath, createdBy) // upstream (unchanged)
	if err != nil {
		return "", errors.Wrapf(err, "creating layer '%s'", layer.ID)
	}
	layer.Source = incomingSource                                     // ADDED: restore, unconditionally
	// ... upstream reuse/add logic unchanged ...
}
```

- `incomingSource := layer.Source` captures the caller's real source before
  re-derivation.
- `layer.Source = incomingSource` restores it **unconditionally** after. This is
  correct for both cases:
  - Filesystem-backed layers (app / buildpack / launcher): restores the real
    source dir (or file), so the consumer copies from the built path.
  - SYNTHESIZED layers (e.g. process-types): `incomingSource` is `nil`, so
    `Source` stays `nil`, and the consumer falls back to its small-tree extraction
    path. Restoring nil is exactly what we want here.

The subsequent `recordLayerSource(image, layer)` call (also fork-added) then
attaches the source to the working image — but only when it is a
`RecordingImage`.

## Why the change is backward compatible (existing usage will NOT break)

The change is safe on the normal (non-BuildKit) export path for three reasons:

1. **`Layer.Source` is a new fork-added, nullable field.** Upstream code never
   sets it. On the normal export path it is always `nil`, so `incomingSource` is
   nil and restoring it (`layer.Source = nil`) is a no-op with respect to upstream
   behavior.

2. **The bytes that matter are untouched.** The layer's `Digest`, `History`, and
   `TarPath` — everything `ReuseLayerWithHistory` / `AddLayerWithDiffIDAndHistory`
   consume — come from the upstream `DirLayer` re-derivation exactly as before.
   Only the new `Source` field is manipulated. The produced image is
   byte-identical to upstream.

3. **`recordLayerSource` is double-guarded and a no-op off the BuildKit path.**
   It returns early if `layer.Source == nil` (always true on the normal path) AND
   returns early if the working image is not an `emit.LayerSourceRecorder`
   (ordinary `imgutil.Image` implementations are not). So it does nothing during
   a normal export.

Forward compatible: on the BuildKit-native emit path the fix is precisely what
makes native `llb.Copy` assembly work (real source preserved; synthesized layers
left sourceless for the small-tree fallback).

**Conclusion: the change is both forward- and backward-compatible. Existing
usage will not break.** If backward compatibility ever became false, this
document would say so explicitly — it does not, and it is not.

## Related fork-added plumbing (context, not part of this fix)

- `layers/factory.go`: new `Layer.Source *LayerSource` field + `LayerSource`
  struct (`Dir`, `File`, `Include`, `Dest`, `UID`, `GID`, `Mode`).
- `layers/{dir,launcher,slices}.go`: populate `Source` when building layers.
- `phase/exporter.go`: `recordLayerSource(image, layer)` helper (type-asserts
  `emit.LayerSourceRecorder`), called on the ADD paths
  (`addOrReuseBuildpackLayer`, `addExtensionLayer`, app-slice add).
- `phase/emit/recording_image.go`: `LayerSourceRecorder` interface +
  `RecordingImage.RecordLayerSource`, consumed only in BuildKit-native emit mode.

## PR guidance

- Describe this as a bug fix that preserves a caller-provided `Layer.Source`
  across the upstream `DirLayer` re-derivation.
- State explicitly that the normal export path is byte-identical to upstream and
  the change is backward compatible (guarded no-op off the BuildKit path).
- No RFC change needed for this specific fix.
