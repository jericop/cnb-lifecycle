package emit_test

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildpacks/imgutil/fakes"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle/phase/emit"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

func TestRecordingImage(t *testing.T) {
	spec.Run(t, "RecordingImage", testRecordingImage, spec.Report(report.Terminal{}))
}

func testRecordingImage(t *testing.T, when spec.G, it spec.S) {
	var (
		tmpDir    string
		outputDir string
	)

	it.Before(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "emit-recording-test")
		h.AssertNil(t, err)
		outputDir = filepath.Join(tmpDir, "out")
	})

	it.After(func() {
		_ = os.RemoveAll(tmpDir)
	})

	newImage := func() *emit.RecordingImage {
		run := fakes.NewImage("run/image", "sha256:runtoplayer", nil)
		return emit.NewRecordingImage(run, "run/image@sha256:run", outputDir)
	}

	readPlan := func() emit.Plan {
		data, err := os.ReadFile(filepath.Join(outputDir, emit.RecorderDir, emit.PlanFileName))
		h.AssertNil(t, err)
		var p emit.Plan
		h.AssertNil(t, json.Unmarshal(data, &p))
		return p
	}

	when("a filesystem-backed layer with a recorded Source", func() {
		it("emits the Source ref and drops the tar", func() {
			img := newImage()
			// exporter adds the layer by its tar path, then records the fs source keyed by tar path.
			tarPath := "/tmp/does-not-need-to-exist/app.tar"
			h.AssertNil(t, img.AddLayerWithDiffIDAndHistory(tarPath, "sha256:app", v1.History{CreatedBy: "app-layer"}))
			img.RecordLayerSource(tarPath, emit.LayerSource{Dir: "/workspace", UID: 1000, GID: 1000})

			h.AssertNil(t, img.Save())

			plan := readPlan()
			h.AssertEq(t, len(plan.Layers), 1)
			l := plan.Layers[0]
			h.AssertEq(t, l.Reused, false)
			h.AssertEq(t, l.TarPath, "") // tar dropped
			if l.Source == nil {
				t.Fatalf("expected Source to be set for filesystem-backed layer")
			}
			h.AssertEq(t, l.Source.Dir, "/workspace")
			h.AssertEq(t, l.Source.UID, 1000)
			h.AssertEq(t, l.Source.GID, 1000)
		})
	})

	when("a synthesized layer with NO recorded Source", func() {
		it("persists a small tar AND extracts a .d tree", func() {
			img := newImage()

			// Build a tiny real tar (a dir + a file + a symlink), mimicking process-types.
			realTar := filepath.Join(tmpDir, "process-types.tar")
			writeTinyTar(t, realTar)

			h.AssertNil(t, img.AddLayerWithDiffIDAndHistory(realTar, "sha256:proc", v1.History{CreatedBy: "process-types"}))
			// NO RecordLayerSource call -> synthesized.

			h.AssertNil(t, img.Save())

			plan := readPlan()
			h.AssertEq(t, len(plan.Layers), 1)
			l := plan.Layers[0]
			if l.Source != nil {
				t.Fatalf("expected no Source for synthesized layer, got %+v", l.Source)
			}
			// TarPath is rewritten to a RELATIVE path under RecorderDir/LayersSubdir.
			if l.TarPath == "" {
				t.Fatalf("expected a persisted tar path for synthesized layer")
			}
			persistedTar := filepath.Join(outputDir, l.TarPath)
			h.AssertPathExists(t, persistedTar)

			// The extracted tree (.d) must exist next to the persisted tar and contain
			// the extracted entries.
			treeDir := persistedTar[:len(persistedTar)-len(".tar")] + ".d"
			h.AssertPathExists(t, treeDir)
			h.AssertPathExists(t, filepath.Join(treeDir, "cnb", "process.txt"))
			// symlink extracted
			li, err := os.Lstat(filepath.Join(treeDir, "cnb", "web"))
			h.AssertNil(t, err)
			h.AssertEq(t, li.Mode()&os.ModeSymlink != 0, true)
		})
	})

	when("Save", func() {
		it("writes plan.json, config.json, and build-metadata.json", func() {
			img := newImage()
			h.AssertNil(t, img.SetLabel("io.buildpacks.lifecycle.metadata", `{"app":[{"sha":"sha256:app"}]}`))
			h.AssertNil(t, img.AddLayerWithDiffIDAndHistory("/tmp/app.tar", "sha256:app", v1.History{CreatedBy: "app-layer"}))
			img.RecordLayerSource("/tmp/app.tar", emit.LayerSource{Dir: "/workspace"})

			h.AssertNil(t, img.Save())

			base := filepath.Join(outputDir, emit.RecorderDir)
			h.AssertPathExists(t, filepath.Join(base, emit.PlanFileName))
			h.AssertPathExists(t, filepath.Join(base, emit.ConfigFileName))
			bmPath := filepath.Join(base, emit.BuildMetadataFileName)
			h.AssertPathExists(t, bmPath)

			// build-metadata.json parses and carries the label + persisted plan.
			data, err := os.ReadFile(bmPath)
			h.AssertNil(t, err)
			bm, err := emit.ParseBuildMetadata(string(data))
			h.AssertNil(t, err)
			h.AssertEq(t, bm.Labels["io.buildpacks.lifecycle.metadata"], `{"app":[{"sha":"sha256:app"}]}`)
			h.AssertEq(t, bm.Plan.RunImage.TopLayer, "sha256:runtoplayer")
			h.AssertEq(t, len(bm.Plan.Layers), 1)
			// the persisted plan inside build-metadata carries the Source ref.
			if bm.Plan.Layers[0].Source == nil {
				t.Fatalf("expected Source ref in build-metadata plan")
			}
			h.AssertEq(t, bm.Plan.Layers[0].Source.Dir, "/workspace")
		})
	})

	when("reused layers", func() {
		it("are recorded by digest with no tar and no source", func() {
			img := newImage()
			h.AssertNil(t, img.ReuseLayerWithHistory("sha256:base", v1.History{CreatedBy: "base-layer"}))
			h.AssertNil(t, img.Save())

			plan := readPlan()
			h.AssertEq(t, len(plan.Layers), 1)
			h.AssertEq(t, plan.Layers[0].Reused, true)
			h.AssertEq(t, plan.Layers[0].TarPath, "")
			if plan.Layers[0].Source != nil {
				t.Fatalf("reused layer should have no Source")
			}
		})
	})
}

// writeTinyTar writes a minimal tar containing a dir, a regular file, and a
// symlink — mimicking a synthesized process-types layer.
func writeTinyTar(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	h.AssertNil(t, err)
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	h.AssertNil(t, tw.WriteHeader(&tar.Header{
		Name: "cnb/", Typeflag: tar.TypeDir, Mode: 0755,
	}))
	content := []byte("web\n")
	h.AssertNil(t, tw.WriteHeader(&tar.Header{
		Name: "cnb/process.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(content)),
	}))
	_, err = tw.Write(content)
	h.AssertNil(t, err)
	h.AssertNil(t, tw.WriteHeader(&tar.Header{
		Name: "cnb/web", Typeflag: tar.TypeSymlink, Linkname: "process.txt", Mode: 0777,
	}))
}
