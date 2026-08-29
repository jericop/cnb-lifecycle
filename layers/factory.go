// Package layers provides a factory that can be used to construct layer tarballs
// from buildpack layer directories, extension layers, and other CNB filesystem constructs.
// The tarballs can be added as layers to any OCI image.
package layers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/buildpacks/lifecycle/archive"
	"github.com/buildpacks/lifecycle/log"
)

const (
	AppLayerName            = "Application Layer"
	BuildpackLayerName      = "Layer: '%s', Created by buildpack: %s"
	ExtensionLayerName      = "Layer: '%s', Created by extension: %s"
	LauncherConfigLayerName = "Buildpacks Launcher Config"
	LauncherLayerName       = "Buildpacks Application Launcher"
	ProcessTypesLayerName   = "Buildpacks Process Types"
	SBOMLayerName           = "Software Bill-of-Materials"
	SliceLayerName          = "Application Slice: %d"
	processing              = "processing"
)

type Factory struct {
	ArtifactsDir string // ArtifactsDir is the directory where layer files are written
	UID, GID     int    // UID and GID are used to normalize layer entries
	Logger       log.Logger
	Ctx          context.Context
	tarHashes    sync.Map // tarHashes Stores hashes of layer tarballs for reuse between the export and cache steps.
}

type Layer struct {
	ID      string
	TarPath string
	Digest  string
	History v1.History
	// Source, when set, describes the FILESYSTEM SOURCE this layer was built from
	// (the dir/file the factory tarred, plus normalization). It lets a consumer
	// (BuildKit-native export emit-mode) assemble the layer with a native copy
	// instead of extracting the tar. It is informational and does NOT affect the
	// tar the factory builds — normal export ignores it. Nil for synthesized
	// layers with no filesystem source (e.g. process-types symlinks).
	Source *LayerSource
}

// LayerSource describes the filesystem source of a Layer for consumers that
// assemble by copy rather than by tar. See Layer.Source.
type LayerSource struct {
	Dir     string   // directory to copy from (buildpack/app/config layers)
	File    string   // single file to copy (launcher binary)
	Include []string // optional: restrict to these paths (app slices)
	Dest    string   // destination if different from source (launcher)
	UID     int      // ownership normalization applied by the factory
	GID     int
	Mode    int64 // optional file mode (launcher: 0755)
}

func (f *Factory) writeLayer(id, createdBy string, addEntries func(tw *archive.NormalizingTarWriter) error) (layer Layer, err error) {
	if f.Ctx == nil {
		f.Ctx = context.TODO()
	}
	tarPath := filepath.Join(f.ArtifactsDir, escape(id)+".tar")
	for {
		sha, loaded := f.tarHashes.LoadOrStore(tarPath, processing)
		if loaded {
			select {
			case <-f.Ctx.Done():
				return Layer{}, f.Ctx.Err()
			default:
				shaString := sha.(string)
				if shaString == processing {
					// another goroutine is processing this layer, wait and try again
					time.Sleep(500 * time.Millisecond)
					continue
				}

				f.Logger.Debugf("Reusing tarball for layer %q with SHA: %s\n", id, shaString)
				return Layer{
					ID:      id,
					TarPath: tarPath,
					Digest:  shaString,
					History: v1.History{CreatedBy: createdBy},
				}, nil
			}
		}
		break
	}

	select {
	case <-f.Ctx.Done():
		return Layer{}, f.Ctx.Err()
	default:
		lw, err := newFileLayerWriter(tarPath)
		if err != nil {
			return Layer{}, err
		}
		defer func() {
			if closeErr := lw.Close(); err == nil {
				err = closeErr
			}
		}()
		tw := tarWriter(lw)
		if err := addEntries(tw); err != nil {
			return Layer{}, err
		}

		if err := tw.Close(); err != nil {
			return Layer{}, err
		}
		digest := lw.Digest()
		f.tarHashes.Store(tarPath, digest)
		return Layer{
			ID:      id,
			Digest:  digest,
			TarPath: tarPath,
			History: v1.History{CreatedBy: createdBy},
		}, err
	}
}

func escape(id string) string {
	return strings.ReplaceAll(id, "/", "_")
}

func parents(file string) ([]archive.PathInfo, error) {
	parent := filepath.Dir(file)
	if parent == filepath.VolumeName(file)+`\` || parent == "/" {
		return []archive.PathInfo{}, nil
	}
	fi, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	parentDirs, err := parents(parent)
	if err != nil {
		return nil, err
	}
	return append(parentDirs, archive.PathInfo{
		Path: parent,
		Info: fi,
	}), nil
}
