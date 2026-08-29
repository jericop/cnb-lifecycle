package layers

import (
	"path/filepath"

	"github.com/buildpacks/lifecycle/archive"
)

// DirLayer creates a layer from the given directory
// DirLayer will set the UID and GID of entries describing dir and its children (but not its parents)
//
//	to Factory.UID and Factory.GID
func (f *Factory) DirLayer(withID string, fromDir string, createdBy string) (layer Layer, err error) {
	fromDir, err = filepath.Abs(fromDir)
	if err != nil {
		return Layer{}, err
	}
	parents, err := parents(fromDir)
	if err != nil {
		return Layer{}, err
	}
	layer, err = f.writeLayer(withID, createdBy, func(tw *archive.NormalizingTarWriter) error {
		if err := archive.AddFilesToArchive(tw, parents); err != nil {
			return err
		}
		tw.WithUID(f.UID)
		tw.WithGID(f.GID)
		return archive.AddDirToArchive(tw, fromDir)
	})
	if err != nil {
		return Layer{}, err
	}
	// Record the filesystem source so a copy-based consumer can assemble this
	// layer from fromDir (with the same uid/gid normalization) instead of the tar.
	layer.Source = &LayerSource{Dir: fromDir, UID: f.UID, GID: f.GID}
	return layer, nil
}
