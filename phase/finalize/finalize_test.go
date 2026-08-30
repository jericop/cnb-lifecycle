package finalize_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	ggcrname "github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle/phase/emit"
	"github.com/buildpacks/lifecycle/phase/finalize"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

func TestFinalize(t *testing.T) {
	spec.Run(t, "Finalize", testFinalize, spec.Report(report.Terminal{}))
}

// lifecycleMetadataLabel is the final CNB metadata label finalize authors.
const lifecycleMetadataLabel = "io.buildpacks.lifecycle.metadata"

func testFinalize(t *testing.T, when spec.G, it spec.S) {
	var (
		srv      *httptest.Server
		regHost  string
		ctx      = context.Background()
	)

	it.Before(func() {
		srv = httptest.NewServer(registry.New())
		u, err := url.Parse(srv.URL)
		h.AssertNil(t, err)
		regHost = u.Host // e.g. 127.0.0.1:PORT
	})

	it.After(func() {
		srv.Close()
	})

	// pushImage pushes img to <regHost>/<repo>:latest (insecure) and returns the ref string.
	pushImage := func(repo string, img v1.Image) string {
		refStr := fmt.Sprintf("%s/%s:latest", regHost, repo)
		ref, err := ggcrname.ParseReference(refStr, ggcrname.Insecure)
		h.AssertNil(t, err)
		h.AssertNil(t, remote.Write(ref, img, remote.WithContext(ctx)))
		return refStr
	}

	// readLabels pulls the image at refStr and returns its config labels.
	readLabels := func(refStr string) map[string]string {
		ref, err := ggcrname.ParseReference(refStr, ggcrname.Insecure)
		h.AssertNil(t, err)
		img, err := remote.Image(ref, remote.WithContext(ctx))
		h.AssertNil(t, err)
		cfg, err := img.ConfigFile()
		h.AssertNil(t, err)
		return cfg.Config.Labels
	}

	// buildImageWithMetadata creates an image with numLayers real random layers,
	// then attaches a build-metadata label whose plan marks the LAST newCount
	// layers as new (intended diffIDs = placeholders) and whose lifecycle-metadata
	// label references those intended diffIDs. Returns (image, intendedToProduced).
	buildImageWithMetadata := func(numLayers, newCount int) (v1.Image, map[string]string) {
		img, err := random.Image(256, int64(numLayers))
		h.AssertNil(t, err)
		cfg, err := img.ConfigFile()
		h.AssertNil(t, err)
		produced := cfg.RootFS.DiffIDs
		producedNew := produced[len(produced)-newCount:]

		// Intended (pre-produce) diffIDs are arbitrary placeholders distinct from produced.
		layersPlan := make([]emit.LayerOp, 0, numLayers)
		intendedToProduced := map[string]string{}
		reusedCount := numLayers - newCount
		for i := 0; i < reusedCount; i++ {
			layersPlan = append(layersPlan, emit.LayerOp{
				ID: fmt.Sprintf("base-%d", i), Reused: true, DiffID: produced[i].String(),
			})
		}
		var lifecycleMeta strings.Builder
		lifecycleMeta.WriteString("{")
		for i := 0; i < newCount; i++ {
			intended := fmt.Sprintf("sha256:%064x", 1000+i)
			layersPlan = append(layersPlan, emit.LayerOp{
				ID: fmt.Sprintf("new-%d", i), Reused: false, DiffID: intended,
			})
			intendedToProduced[intended] = producedNew[i].String()
			if i > 0 {
				lifecycleMeta.WriteString(",")
			}
			// reference the intended diffID inside the label value (finalize must remap it)
			lifecycleMeta.WriteString(fmt.Sprintf("%q:%q", fmt.Sprintf("layer%d", i), intended))
		}
		lifecycleMeta.WriteString("}")

		bm := emit.BuildMetadata{
			Schema: emit.Schema,
			Plan: emit.Plan{
				Schema:   emit.Schema,
				RunImage: emit.PlanRunImage{Reference: "run@sha256:run", TopLayer: produced[0].String()},
				Layers:   layersPlan,
			},
			Labels: map[string]string{lifecycleMetadataLabel: lifecycleMeta.String()},
		}
		bmJSON, err := bm.Marshal()
		h.AssertNil(t, err)

		img, err = mutate.Config(img, v1.Config{
			Labels: map[string]string{emit.BuildMetadataLabel: bmJSON},
		})
		h.AssertNil(t, err)
		return img, intendedToProduced
	}

	when("a buildkit-native image with a build-metadata label", func() {
		it("authors lifecycle.metadata with produced diffIDs and drops the build-metadata label", func() {
			img, intendedToProduced := buildImageWithMetadata(4, 2)
			ref := pushImage("app/finalize-basic", img)

			err := finalize.Finalize(ctx, ref, finalize.Options{Insecure: true})
			h.AssertNil(t, err)

			labels := readLabels(ref)
			// build-metadata label removed by default.
			_, has := labels[emit.BuildMetadataLabel]
			h.AssertEq(t, has, false)

			// lifecycle.metadata authored: every intended diffID replaced by its produced diffID.
			meta := labels[lifecycleMetadataLabel]
			h.AssertEq(t, meta != "", true)
			for intended, produced := range intendedToProduced {
				h.AssertStringContains(t, meta, produced)
				if strings.Contains(meta, intended) {
					t.Fatalf("expected intended diffID %s to be remapped, but it is still present in %s", intended, meta)
				}
			}
		})

		it("keeps the build-metadata label when KeepBuildMetadataLabel is set", func() {
			img, _ := buildImageWithMetadata(3, 1)
			ref := pushImage("app/finalize-keep", img)

			err := finalize.Finalize(ctx, ref, finalize.Options{Insecure: true, KeepBuildMetadataLabel: true})
			h.AssertNil(t, err)

			labels := readLabels(ref)
			_, has := labels[emit.BuildMetadataLabel]
			h.AssertEq(t, has, true)
		})
	})

	when("idempotency", func() {
		it("a second finalize is a no-op (label already gone)", func() {
			img, intendedToProduced := buildImageWithMetadata(4, 2)
			ref := pushImage("app/finalize-idem", img)

			h.AssertNil(t, finalize.Finalize(ctx, ref, finalize.Options{Insecure: true}))
			first := readLabels(ref)[lifecycleMetadataLabel]

			// second run: build-metadata label is gone, so it is a no-op.
			h.AssertNil(t, finalize.Finalize(ctx, ref, finalize.Options{Insecure: true}))
			second := readLabels(ref)[lifecycleMetadataLabel]

			h.AssertEq(t, first, second)
			for _, produced := range intendedToProduced {
				h.AssertStringContains(t, second, produced)
			}
		})

		it("re-finalizing a kept-label image authors identical metadata", func() {
			img, _ := buildImageWithMetadata(4, 2)
			ref := pushImage("app/finalize-keep-idem", img)

			h.AssertNil(t, finalize.Finalize(ctx, ref, finalize.Options{Insecure: true, KeepBuildMetadataLabel: true}))
			first := readLabels(ref)[lifecycleMetadataLabel]
			h.AssertNil(t, finalize.Finalize(ctx, ref, finalize.Options{Insecure: true, KeepBuildMetadataLabel: true}))
			second := readLabels(ref)[lifecycleMetadataLabel]
			h.AssertEq(t, first, second)
		})
	})

	when("an image without a build-metadata label", func() {
		it("is a no-op (not a buildkit-native image / already finalized)", func() {
			img, err := random.Image(256, 3)
			h.AssertNil(t, err)
			ref := pushImage("app/no-label", img)

			err = finalize.Finalize(ctx, ref, finalize.Options{Insecure: true})
			h.AssertNil(t, err)

			labels := readLabels(ref)
			_, has := labels[lifecycleMetadataLabel]
			h.AssertEq(t, has, false) // nothing authored
		})
	})

	when("the build-metadata lists more new layers than the image has", func() {
		it("returns an error", func() {
			// 2 layers but claim 3 new layers.
			img, err := random.Image(256, 2)
			h.AssertNil(t, err)
			bm := emit.BuildMetadata{
				Schema: emit.Schema,
				Plan: emit.Plan{
					Schema: emit.Schema,
					Layers: []emit.LayerOp{
						{ID: "n0", Reused: false, DiffID: "sha256:0000000000000000000000000000000000000000000000000000000000000001"},
						{ID: "n1", Reused: false, DiffID: "sha256:0000000000000000000000000000000000000000000000000000000000000002"},
						{ID: "n2", Reused: false, DiffID: "sha256:0000000000000000000000000000000000000000000000000000000000000003"},
					},
				},
				Labels: map[string]string{lifecycleMetadataLabel: "{}"},
			}
			bmJSON, err := bm.Marshal()
			h.AssertNil(t, err)
			img, err = mutate.Config(img, v1.Config{Labels: map[string]string{emit.BuildMetadataLabel: bmJSON}})
			h.AssertNil(t, err)
			ref := pushImage("app/too-many-new", img)

			err = finalize.Finalize(ctx, ref, finalize.Options{Insecure: true})
			h.AssertError(t, err, "new layers")
		})
	})
}
