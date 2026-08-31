package phase

import (
	"testing"

	"github.com/buildpacks/imgutil/fakes"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle/layers"
	"github.com/buildpacks/lifecycle/phase/emit"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

// This is an INTERNAL test (package phase) so it can exercise the unexported
// recordLayerSource helper, which is the seam that carries the fork's BuildKit-
// native layer Source through to emit-mode. It verifies:
//   - a layer WITH a Source is recorded (keyed by TarPath) on a RecordingImage-like
//     LayerSourceRecorder, and
//   - a layer with a NIL Source records nothing (the normal-export backward-compat
//     no-op, and the synthesized-layer case that must fall back to a tar/tree).
//
// See .kiro/steering/exporter-preserve-layer-source.md.

func TestExporterSourcePreservation(t *testing.T) {
	spec.Run(t, "ExporterSourcePreservation", testExporterSourcePreservation, spec.Report(report.Terminal{}))
}

// fakeRecorder is a real imgutil.Image (via the embedded *fakes.Image) that ALSO
// implements LayerSourceRecorder — mirroring emit.RecordingImage. recordLayerSource
// takes an imgutil.Image and type-asserts LayerSourceRecorder, so the recorder must
// satisfy both. It captures RecordLayerSource calls.
type fakeRecorder struct {
	*fakes.Image
	recorded map[string]emit.LayerSource
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{Image: fakes.NewImage("app/emit", "", nil)}
}

func (f *fakeRecorder) RecordLayerSource(tarPath string, src emit.LayerSource) {
	if f.recorded == nil {
		f.recorded = map[string]emit.LayerSource{}
	}
	f.recorded[tarPath] = src
}

func testExporterSourcePreservation(t *testing.T, when spec.G, it spec.S) {
	when("a layer carries a filesystem Source", func() {
		it("records the source keyed by the layer's TarPath", func() {
			rec := newFakeRecorder()
			layer := layers.Layer{
				ID:      "some-buildpack:some-layer",
				TarPath: "/tmp/artifacts/some-layer.tar",
				Digest:  "sha256:abc",
				Source: &layers.LayerSource{
					Dir: "/layers/some-buildpack/some-layer",
					UID: 1234,
					GID: 5678,
				},
			}

			recordLayerSource(rec, layer)

			src, ok := rec.recorded[layer.TarPath]
			h.AssertEq(t, ok, true)
			h.AssertEq(t, src.Dir, "/layers/some-buildpack/some-layer")
			h.AssertEq(t, src.UID, 1234)
			h.AssertEq(t, src.GID, 5678)
		})

		it("carries File/Dest/Mode/Include through for launcher and slice layers", func() {
			rec := newFakeRecorder()
			layer := layers.Layer{
				ID:      "launcher",
				TarPath: "/tmp/artifacts/launcher.tar",
				Source: &layers.LayerSource{
					File: "/cnb/lifecycle/launcher",
					Dest: "/cnb/lifecycle/launcher",
					Mode: 0755,
				},
			}
			recordLayerSource(rec, layer)
			src := rec.recorded[layer.TarPath]
			h.AssertEq(t, src.File, "/cnb/lifecycle/launcher")
			h.AssertEq(t, src.Dest, "/cnb/lifecycle/launcher")
			h.AssertEq(t, src.Mode, int64(0755))

			rec2 := newFakeRecorder()
			slice := layers.Layer{
				ID:      "slice-1",
				TarPath: "/tmp/artifacts/slice-1.tar",
				Source:  &layers.LayerSource{Dir: "/workspace", Include: []string{"a.txt", "b/c.txt"}},
			}
			recordLayerSource(rec2, slice)
			h.AssertEq(t, rec2.recorded[slice.TarPath].Include, []string{"a.txt", "b/c.txt"})
		})
	})

	when("a layer has NO Source (synthesized layer / normal export)", func() {
		it("records nothing (backward-compatible no-op)", func() {
			rec := newFakeRecorder()
			layer := layers.Layer{
				ID:      "process-types",
				TarPath: "/tmp/artifacts/process-types.tar",
				Digest:  "sha256:proc",
				Source:  nil,
			}

			recordLayerSource(rec, layer)

			h.AssertEq(t, len(rec.recorded), 0)
		})
	})

	when("the working image is not a LayerSourceRecorder (normal export path)", func() {
		it("does nothing even when a Source is present", func() {
			// A real imgutil.Image that does NOT implement LayerSourceRecorder:
			// recordLayerSource must be a no-op (does not panic, records nothing).
			// This is the byte-identical-to-upstream normal export path.
			nonRecorder := fakes.NewImage("app/normal-export", "", nil)
			layer := layers.Layer{
				ID:      "app",
				TarPath: "/tmp/artifacts/app.tar",
				Source:  &layers.LayerSource{Dir: "/workspace"},
			}
			// Must not panic and must not add any layer to the non-recorder image.
			recordLayerSource(nonRecorder, layer)
			h.AssertEq(t, nonRecorder.NumberOfAddedLayers(), 0)
		})
	})
}
