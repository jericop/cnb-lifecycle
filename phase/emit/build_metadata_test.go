package emit_test

import (
	"strings"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle/phase/emit"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

func TestBuildMetadata(t *testing.T) {
	spec.Run(t, "BuildMetadata", testBuildMetadata, spec.Report(report.Terminal{}))
}

func testBuildMetadata(t *testing.T, when spec.G, it spec.S) {
	when("Marshal / ParseBuildMetadata round-trip", func() {
		it("preserves plan, labels, and runtime config", func() {
			in := emit.BuildMetadata{
				Schema: emit.Schema,
				Plan: emit.Plan{
					Schema: emit.Schema,
					RunImage: emit.PlanRunImage{
						Reference: "run@sha256:aaa",
						TopLayer:  "sha256:top",
					},
					Layers: []emit.LayerOp{
						{ID: "base", Reused: true, DiffID: "sha256:base"},
						{ID: "app", Reused: false, DiffID: "sha256:app"},
					},
				},
				Labels:     map[string]string{"io.buildpacks.lifecycle.metadata": `{"x":"sha256:app"}`},
				Env:        map[string]string{"PATH": "/cnb/bin"},
				Entrypoint: []string{"/cnb/process/web"},
				Cmd:        []string{"--flag"},
				WorkingDir: "/workspace",
			}

			s, err := in.Marshal()
			h.AssertNil(t, err)

			out, err := emit.ParseBuildMetadata(s)
			h.AssertNil(t, err)
			h.AssertEq(t, out, in)
		})
	})

	when("ParseBuildMetadata validation", func() {
		it("rejects an unsupported schema", func() {
			_, err := emit.ParseBuildMetadata(`{"schema":"bogus/v0","plan":{"layers":[{"id":"a"}]}}`)
			h.AssertError(t, err, "unsupported")
		})

		it("rejects a plan with no layers", func() {
			_, err := emit.ParseBuildMetadata(`{"schema":"` + emit.Schema + `","plan":{"layers":[]}}`)
			h.AssertError(t, err, "no layers")
		})

		it("rejects invalid JSON", func() {
			_, err := emit.ParseBuildMetadata(`{not json`)
			h.AssertError(t, err, "parse build-metadata")
		})
	})

	when("NewLayerDiffIDs", func() {
		it("returns only NON-reused layer diffIDs in plan order", func() {
			bm := emit.BuildMetadata{
				Schema: emit.Schema,
				Plan: emit.Plan{
					Layers: []emit.LayerOp{
						{ID: "base", Reused: true, DiffID: "sha256:base"},
						{ID: "launcher", Reused: false, DiffID: "sha256:launcher"},
						{ID: "reused-bp", Reused: true, DiffID: "sha256:reused"},
						{ID: "app", Reused: false, DiffID: "sha256:app"},
					},
				},
			}
			got := bm.NewLayerDiffIDs()
			h.AssertEq(t, got, []string{"sha256:launcher", "sha256:app"})
		})

		it("returns nil when all layers are reused", func() {
			bm := emit.BuildMetadata{Plan: emit.Plan{Layers: []emit.LayerOp{
				{ID: "base", Reused: true, DiffID: "sha256:base"},
			}}}
			got := bm.NewLayerDiffIDs()
			h.AssertEq(t, len(got), 0)
		})
	})

	when("NewBuildMetadata", func() {
		it("copies config labels/env/entrypoint and stamps the schema", func() {
			plan := emit.Plan{Schema: emit.Schema, Layers: []emit.LayerOp{{ID: "app", DiffID: "sha256:app"}}}
			cfg := emit.ImageConfig{
				Labels:     map[string]string{"k": "v"},
				Env:        map[string]string{"PATH": "/x"},
				Entrypoint: []string{"/cnb/process/web"},
				Cmd:        []string{},
				WorkingDir: "/workspace",
			}
			bm := emit.NewBuildMetadata(plan, cfg)
			h.AssertEq(t, bm.Schema, emit.Schema)
			h.AssertEq(t, bm.Labels, cfg.Labels)
			h.AssertEq(t, bm.Env, cfg.Env)
			h.AssertEq(t, bm.Entrypoint, cfg.Entrypoint)
			h.AssertEq(t, bm.WorkingDir, cfg.WorkingDir)

			// round-trips through the label value.
			s, err := bm.Marshal()
			h.AssertNil(t, err)
			h.AssertEq(t, strings.Contains(s, emit.Schema), true)
		})
	})
}
