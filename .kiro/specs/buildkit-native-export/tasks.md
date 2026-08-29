# Tasks: Lifecycle BuildKit-Native Export (emit-mode + finalize)

Lifecycle-side half of Option A (build-then-finalize). Additive, opt-in. The
pack-side half is `jericop/cnb-pack@buildkit-native-export`. Local/MVP validation
with repeated cycles; no `PACK_TEST_*` gates.

Branch: `jericop/cnb-lifecycle@buildkit-native-export`.

## Design summary (see design.md + the spike decision record)

The lifecycle provides: (1) emit-mode that computes the ordered layer plan and
surfaces it as the `io.buildpacks.buildkit.native.build-metadata` LABEL, and (2) a
FINALIZE library API (+ subcommand) that authors `io.buildpacks.lifecycle.metadata`
on a built+pushed image from its ACTUAL produced diffIDs + that label, re-pushing
config+manifest only. No frontend, no per-layer re-extraction, no post-push layer
changes.

## SUPERSEDED (previous MVP — retained for history)

The RecordingImage plan COMPUTATION is retained. Superseded: the file-based
transport (`plan.json`/`config.json`), the persisted layer tars under
`buildkit/layers/`, the `io.buildpacks.native.layer-order` temp label, and the
custom frontend as the assembly mechanism. The plan is now a LABEL; metadata is
AUTHORED by finalize, not patched.

## Tasks

- [x] 1. Emit-mode computes the ordered layer plan
  - DONE (retained). `phase/emit` RecordingImage records the ordered layer ops +
    config the exporter performs (reuse of `layers.Factory` + `files.LayersMetadata`).
    This is the plan source.
  - _Requirements: 1, 2_

- [ ] 2. Surface the plan as the build-metadata label (drop file/tar transport)
  - Serialize the ordered plan (layers: order, new-vs-reused, intended diffID,
    identity, history; runImage reference/topLayer; `schema`) as JSON for the
    `io.buildpacks.buildkit.native.build-metadata` label.
  - Remove the tar-persistence + `plan.json`/`config.json` file transport and the
    `io.buildpacks.native.layer-order` label (superseded). The plan no longer needs
    tar paths (finalize reads produced diffIDs from the image, not tars).
  - _Requirements: 2, 3_

- [ ] 3. Finalize library API: author metadata from a built image
  - New importable package (e.g. `phase/finalize`) + a `finalize` subcommand. Given
    an image ref (single or manifest list): read produced diffIDs (image config) +
    the build-metadata label, map plan entries → produced diffIDs positionally,
    build `files.LayersMetadata` with per-layer SHAs = produced diffIDs +
    RunImage boundary, author the other export labels, set them on the config, and
    re-push config+manifest(+index) only. Idempotent + tag-atomic.
  - Consumable in-process by pack like `phase.Rebaser`. Use go-containerregistry for
    image access (as rebase does).
  - _Requirements: 4, 5, 6_

- [ ] 4. Wire emit-mode + label into the build (exporter or thin step)
  - Ensure a build can produce a normal image AND attach the build-metadata label
    (preferred: the lifecycle exporter writes it in the build path pack uses). Do NOT
    pre-write a valid final `io.buildpacks.lifecycle.metadata`.
  - _Requirements: 3_

- [ ] 5. Platform API compatibility (>= 0.7)
  - Confirm the plan + finalized metadata are correct across Platform API 0.7–0.15
    (the plan records exactly what the exporter does for the negotiated API; no
    per-version logic). Finalized metadata + `CNB_PLATFORM_API` reflect the API.
  - _Requirements: 7_

- [ ] 6. Publish an emit+finalize-capable lifecycle image (for pack e2e)
  - Build/publish the lifecycle (+ local multi-arch variant) so pack's
    buildkit-native backend can drive the build (emit-mode label) and call finalize
    end to end.
  - _Requirements: 8_

- [ ] 7. Local validation — repeated cycles (with pack)
  - Drive the pack e2e: cold build + ≥2 rebuilds + ≥2 rebases + rebuild-after-rebase
    + multi-arch. Confirm finalize authors correct metadata each time and never
    changes layers.
  - _Requirements: 8_

- [ ] 8. (DEFERRED — after MVP) Automated tests
  - Emit plan serializer, finalize authoring from produced diffIDs, export-parity.
    Local registry like testhelpers; no `PACK_TEST_*` gates.
  - _Requirements: 8_

## Task Dependency Graph

```
1 [DONE: plan computation] ─> 2 (plan as build-metadata label) ─> 3 (finalize library + subcommand) ─> 4 (wire label into build) ─> 5 (platform API) ─> 6 (publish image) ─> 7 (repeated-cycle validation)
                                                                                                                                          │
                                                                                                              (deferred) 8 (automated tests)
```

## Notes

- Finalize is the lifecycle's home for CNB metadata authorship (single source of
  truth); pack consumes it like `phase.Rebaser`. This replaces pack's host-side
  metadata-SHA rewrite.
- The build-metadata label is namespaced `io.buildpacks.buildkit.native.*`, distinct
  from the final CNB metadata label, and may be kept durable on the image to enable
  a future self-healing/repair tool.
