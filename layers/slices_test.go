package layers_test

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle/layers"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

func TestSliceLayers(t *testing.T) {
	spec.Run(t, "Factory", testSlices, spec.Parallel(), spec.Report(report.Terminal{}))
}

func testSlices(t *testing.T, when spec.G, it spec.S) {
	var (
		factory    *layers.Factory
		dirToSlice string
	)
	it.Before(func() {
		var err error
		artifactDir, err := os.MkdirTemp("", "layers.slices.layer")
		h.AssertNil(t, err)
		factory = &layers.Factory{
			ArtifactsDir: artifactDir,
			UID:          1234,
			GID:          4321,
		}
		dirToSlice, err = filepath.Abs(filepath.Join("testdata", "target-dir"))
		h.AssertNil(t, err)
	})

	it.After(func() {
		os.RemoveAll(factory.ArtifactsDir)
	})

	when("#SliceLayers", func() {
		when("there are no slices", func() {
			it("creates a single app layer", func() {
				sliceLayers, err := factory.SliceLayers(dirToSlice, []layers.Slice{})
				h.AssertNil(t, err)
				h.AssertEq(t, len(sliceLayers), 1)
				h.AssertEq(t, sliceLayers[0].ID, "slice-1")
				// parent layers should have uid/gid matching the filesystem
				// the sliced dir and it's children should have normalized uid/gid
				assertTarEntries(t, sliceLayers[0].TarPath, append(parents(t, dirToSlice), []*tar.Header{
					{
						Name:     tarPath(dirToSlice),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "dir-link")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeSymlink,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "file-link.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeSymlink,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir", "other-file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir", "other-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "some-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))
				// it returns history
				h.AssertEq(t, sliceLayers[0].History.CreatedBy, "Application Layer")
			})

			it("resolves relative paths", func() {
				sliceLayers, err := factory.SliceLayers(filepath.Join("testdata", "target-dir"), []layers.Slice{})
				h.AssertNil(t, err)
				h.AssertEq(t, len(sliceLayers), 1)
				h.AssertEq(t, sliceLayers[0].ID, "slice-1")
				assertTarEntries(t, sliceLayers[0].TarPath, append(parents(t, dirToSlice), []*tar.Header{
					{
						Name:     tarPath(dirToSlice),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "dir-link")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeSymlink,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "file-link.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeSymlink,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir", "other-file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir", "other-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "some-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))
			})
		})

		when("there are n slices", func() {
			var sliceLayers []layers.Layer

			it.Before(func() {
				var err error
				sliceLayers, err = factory.SliceLayers(dirToSlice, []layers.Slice{
					{Paths: []string{"*.txt", "**/*.txt"}},
					{Paths: []string{"other-dir"}},
					{Paths: []string{"dir-link/*"}},
					{Paths: []string{"../**/dir-to-exclude"}},
				})
				h.AssertNil(t, err)
			})

			it("creates n+1 layers", func() {
				h.AssertEq(t, len(sliceLayers), 5)
				h.AssertEq(t, sliceLayers[0].ID, "slice-1")
				h.AssertEq(t, sliceLayers[1].ID, "slice-2")
				h.AssertEq(t, sliceLayers[2].ID, "slice-3")
				h.AssertEq(t, sliceLayers[3].ID, "slice-4")
				h.AssertEq(t, sliceLayers[4].ID, "slice-5")
				// it returns history
				h.AssertEq(t, sliceLayers[0].History.CreatedBy, "Application Slice: 1")
			})

			it("creates slice from pattern", func() {
				assertTarEntries(t, sliceLayers[0].TarPath, append(parents(t, dirToSlice), []*tar.Header{
					{
						Name:     tarPath(dirToSlice),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "file-link.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeSymlink,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir", "other-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "some-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))
			})

			it("accepts dirs", func() {
				assertTarEntries(t, sliceLayers[1].TarPath, append(parents(t, dirToSlice), []*tar.Header{
					{
						Name:     tarPath(dirToSlice),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "other-dir", "other-file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))
			})

			it("doesn't glob through symlinks", func() {
				assertTarEntries(t, sliceLayers[2].TarPath, []*tar.Header{})
			})

			it("doesn't glob files outside of dir", func() {
				assertTarEntries(t, sliceLayers[3].TarPath, []*tar.Header{})
			})

			it("creates a layer with the remaining files", func() {
				assertTarEntries(t, sliceLayers[4].TarPath, append(parents(t, dirToSlice), []*tar.Header{
					{
						Name:     tarPath(dirToSlice),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "dir-link")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeSymlink,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))
			})

			it("returns history", func() {
				for idx, s := range sliceLayers {
					h.AssertEq(t, s.History.CreatedBy, fmt.Sprintf("Application Slice: %d", idx+1))
				}
			})
		})

		when("the dir has special characters", func() {
			it("does not treat the dir like a pattern", func() {
				specialCharDir, err := filepath.Abs(filepath.Join("testdata", "target-di[r]"))
				h.AssertNil(t, err)
				sliceLayers, err := factory.SliceLayers(specialCharDir, []layers.Slice{
					{Paths: []string{"*"}},
				})
				h.AssertNil(t, err)
				h.AssertEq(t, len(sliceLayers), 2)
				h.AssertEq(t, sliceLayers[0].ID, "slice-1")
				// parent layers should have uid/gid matching the filesystem
				// the sliced dir and it's children should have normalized uid/gid
				assertTarEntries(t, sliceLayers[0].TarPath, append(parents(t, specialCharDir), []*tar.Header{
					{
						Name:     tarPath(specialCharDir),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(specialCharDir, "special-char-test-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))

				assertTarEntries(t, sliceLayers[1].TarPath, []*tar.Header{})
			})
		})

		when("the pattern ends in a path separator", func() {
			it("matches", func() {
				pattern := "some-dir" + string(filepath.Separator)
				sliceLayers, err := factory.SliceLayers(dirToSlice, []layers.Slice{
					{Paths: []string{pattern}},
				})
				h.AssertNil(t, err)
				h.AssertEq(t, len(sliceLayers), 2)
				h.AssertEq(t, sliceLayers[0].ID, "slice-1")
				assertTarEntries(t, sliceLayers[0].TarPath, append(parents(t, dirToSlice), []*tar.Header{
					{
						Name:     tarPath(dirToSlice),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeDir,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "file.md")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
					{
						Name:     tarPath(filepath.Join(dirToSlice, "some-dir", "some-file.txt")),
						Uid:      factory.UID,
						Gid:      factory.GID,
						Typeflag: tar.TypeReg,
					},
				}...))
			})
		})

		// The BuildKit-native (prepare-image-metadata) path does NOT extract slice
		// tars. Instead it records each slice layer's filesystem Source (the app dir
		// + the EXACT relative paths that slice contains + uid/gid), and the pack
		// consumer reproduces the slice with a native copy using those paths as
		// llb.Copy IncludePatterns. These tests pin that Source contract, which is
		// the emit-side seam of app slicing (see copyFromSource in pack's
		// native_buildfunc.go for the consumer seam).
		when("recording the filesystem Source for slice layers (buildkit-native seam)", func() {
			it("sets Source.Dir + per-slice Include (relative paths) + uid/gid on every layer", func() {
				sliceLayers, err := factory.SliceLayers(dirToSlice, []layers.Slice{
					{Paths: []string{"some-dir"}},        // slice 1: some-dir + its files
					{Paths: []string{"other-dir/*.txt"}}, // slice 2: only the .txt under other-dir
				})
				h.AssertNil(t, err)
				// 2 slices -> 3 layers (slice-1, slice-2, remainder).
				h.AssertEq(t, len(sliceLayers), 3)

				for i, l := range sliceLayers {
					if l.Source == nil {
						t.Fatalf("layer %d (%s) has nil Source; the buildkit-native consumer needs it", i, l.ID)
					}
					// Every slice layer's Source points at the app dir and carries the
					// factory's normalized uid/gid.
					h.AssertEq(t, l.Source.Dir, dirToSlice)
					h.AssertEq(t, l.Source.UID, factory.UID)
					h.AssertEq(t, l.Source.GID, factory.GID)
					// Include paths are RELATIVE to the app dir (never absolute, never ".").
					for _, inc := range l.Source.Include {
						if filepath.IsAbs(inc) {
							t.Fatalf("layer %d Include path %q must be relative to the app dir", i, inc)
						}
						if inc == "." || inc == "" {
							t.Fatalf("layer %d Include path %q is invalid", i, inc)
						}
					}
				}

				// Slice 1 must include some-dir and both its files (dir + children),
				// and MUST NOT include other-dir's files or the top-level files.
				s1 := sliceLayers[0].Source.Include
				h.AssertContains(t, s1, "some-dir", filepath.Join("some-dir", "file.md"), filepath.Join("some-dir", "some-file.txt"))
				h.AssertDoesNotContain(t, s1, filepath.Join("other-dir", "other-file.txt"))
				h.AssertDoesNotContain(t, s1, "file.txt")

				// Slice 2 must include ONLY other-dir/*.txt (plus its parent dir), and
				// MUST NOT include other-dir/other-file.md (not a .txt) or slice-1 files.
				s2 := sliceLayers[1].Source.Include
				h.AssertContains(t, s2, filepath.Join("other-dir", "other-file.txt"))
				h.AssertDoesNotContain(t, s2, filepath.Join("other-dir", "other-file.md"))
				h.AssertDoesNotContain(t, s2, filepath.Join("some-dir", "file.md"))

				// The remainder layer must contain the top-level files not sliced above
				// and MUST NOT re-include slice-1/slice-2 files (each file lands in
				// exactly one layer).
				s3 := sliceLayers[2].Source.Include
				h.AssertContains(t, s3, "file.txt")
				h.AssertDoesNotContain(t, s3, filepath.Join("some-dir", "some-file.txt"))
				h.AssertDoesNotContain(t, s3, filepath.Join("other-dir", "other-file.txt"))
			})
		})
	})
}
