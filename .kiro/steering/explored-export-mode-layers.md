---
inclusion: manual
---

# EXPLORED PATH: Lifecycle `-export-mode layers` Custom Contract

## Status: EXPLORED, DEFERRED

This document preserves a design that was explored but **not chosen** for the initial implementation of eliminating intermediate image tags in BuildKit multi-arch builds.

**Why deferred:** This custom `-export-mode layers` contract was explored but not
chosen. The implementation instead converged on a build-then-finalize design: the
single `buildkit` backend builds and pushes natively in BuildKit, and a lifecycle
`phase/finalize` library authors the CNB metadata post-push (see
`buildkit-changes.md`). The custom contract's main advantage — per-layer BuildKit
caching — provides only marginal benefit because:

1. Buildpacks always re-run when source changes (COPY invalidates downstream)
2. The lifecycle's own `--mount=type=cache` handles dependency caching
3. Registries are content-addressable (unchanged blobs aren't re-pushed)
4. Export time is small relative to total build time

See `cnb-pack/.kiro/steering/buildkit-caching-analysis.md` for the full caching analysis.

**When this may become relevant again:** If we implement the **pre-copy buildpack caching / multi-stage** optimization (see `cnb-pack/.kiro/steering/pre-copy-buildpack-caching.md`), the decomposed layer contract described here becomes valuable. That optimization requires gathering layers produced across multiple build stages and reassembling them into the final image — exactly what this contract enables. The manifest.json format and per-layer decomposition would be the foundation for that work.

---

## Original Design (Preserved for Reference)

### Concept

Add a new export mode to the lifecycle (`-export-mode layers`) that writes decomposed layer output to disk instead of pushing to a registry or loading to a daemon. This output follows a defined contract (manifest.json + layer files) that any container build tool (BuildKit, Buildah, Podman) can consume to assemble the final app image with native caching support.

### Architecture

```
┌─────────────────────────────────────────────────┐
│  Lifecycle Exporter                              │
│  When -export-mode=layers:                       │
│  1. Identify launch layers (same logic as today) │
│  2. Write each layer as expanded directory       │
│  3. Write launcher layer                         │
│  4. Write app layer                              │
│  5. Compute image config (labels, entrypoint)    │
│  6. Write manifest.json                          │
│  7. Write report.toml                            │
│  Output: /output/manifest.json + /output/layers/ │
└─────────────────────────────────────────────────┘
```

### Key Design Decisions

1. **Reuse existing layer selection logic** — The exporter already determines launch layers, computes diff IDs, decides order. Only the destination changes (disk directories vs registry/daemon).

2. **Expanded directories (not tarballs)** — BuildKit/Buildah can COPY directory contents directly (one layer per COPY), no extraction step, BuildKit caches each COPY independently, diff IDs pre-computed in manifest.json.

3. **manifest.json as universal contract** — JSON is universally parseable, OCI configs are already JSON, labels contain JSON strings, easy for pack/buildah/podman to consume.

4. **Layer naming convention** — `{NN}-{buildpack-id-sanitized}--{layer-name}/`. Numeric prefix ensures ordering; manifest.json is authoritative for order.

5. **Isolated integration point** — A new `exportToLayers()` function alongside existing `initRemoteAppImage()`, `initDaemonAppImage()`, `initLayoutAppImage()`.

### manifest.json Schema (v1.0)

```json
{
  "schema_version": "1.0",
  "platform_api": "0.15",
  "run_image": {
    "reference": "string",
    "target": {"os": "string", "arch": "string", "variant": "string"}
  },
  "layers": [
    {
      "path": "layers/00-launcher/",
      "diff_id": "sha256:...",
      "size": 12345,
      "metadata": {
        "type": "launcher|buildpack|app",
        "buildpack_id": "string (empty for launcher/app)",
        "layer_name": "string",
        "cache": false,
        "launch": true,
        "build": false
      }
    }
  ],
  "image_config": {
    "user": "1001:1001",
    "working_dir": "/workspace",
    "entrypoint": ["/cnb/process/web"],
    "cmd": [],
    "env": ["KEY=VALUE"],
    "labels": {"key": "value"},
    "exposed_ports": {"8080/tcp": {}}
  },
  "processes": [
    {"type": "string", "command": ["string"], "args": ["string"], "default": true, "working_dir": "string"}
  ],
  "buildpack_metadata": {
    "buildpacks": [{"id": "string", "version": "string"}]
  },
  "sbom": {"launch_dir": "sbom/launch/", "build_dir": "sbom/build/"}
}
```

### Implementation Plan (Original)

Files to modify:
- `cmd/lifecycle/cli/flags.go` — Add `FlagExportMode()`
- `platform/lifecycle_inputs.go` — Add `ExportMode string` field
- `cmd/lifecycle/exporter.go` — Add `exportToLayers()` path in `Exec()`
- `phase/exporter.go` — Layer decomposition and manifest writing

New files:
- `phase/export_layers.go` — Core logic for decomposed layer export
- `platform/files/layers_manifest.go` — manifest.json types and serialization

### Requirements (Original)

- FR-1: New `-export-mode layers` flag (opt-in)
- FR-2: Output directory structure (manifest.json + layers/ + report.toml)
- FR-3: manifest.json contract (schema version, run image, ordered layers, image config, processes, buildpack metadata; MUST include SBOM if generated per platform spec)
- FR-4: Layer output format (expanded directories, numbered, human-readable names, diff IDs)
- FR-5: Image config in manifest.json (all CNB labels, entrypoint, user, workdir, env, exposed ports)
- FR-6: Backward compatibility (unchanged behavior without the flag)
- FR-7: Integration with `-skip-chown`, `-cache-dir`

### Tasks (Original)

1. Add flag and input definitions
2. Define manifest.json types
3. Implement layer directory writing
4. Implement image config assembly
5. Wire into exporter.go
6. Integration testing
7. Documentation

### Testing Strategy (Original)

- Unit tests for manifest.json serialization/deserialization
- Unit tests for layer directory writing
- Integration test: run full lifecycle in layers mode, verify output can be consumed
- Backward compatibility: existing tests pass unchanged without the new flag

---

## Relationship to the Chosen Approach

The chosen approach is the single builder-agnostic `buildkit` backend
(build-then-finalize): BuildKit builds and pushes the image natively, and the
lifecycle `phase/finalize` library authors the CNB metadata from the produced
diffIDs post-push. It requires no lifecycle changes beyond the already-implemented
`-skip-chown` flag plus the emit-mode/finalize additions (see `buildkit-changes.md`).
The earlier OCI-layout path and its `-pull-run-image` flag were removed.

If pre-copy buildpack caching is pursued later, revisit this document — the
decomposed contract is the natural foundation for reassembling layers produced
across multiple build stages.
