package cachemount

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/progress"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// contentImporter imports cache mount contents from OCI artifacts
type contentImporter struct {
	provider content.Provider
	desc     ocispecs.Descriptor
	ref      string
}

// NewImporter creates a new cache mount importer
func NewImporter(provider content.Provider, desc ocispecs.Descriptor, ref string) Importer {
	return &contentImporter{
		provider: provider,
		desc:     desc,
		ref:      ref,
	}
}

func (i *contentImporter) Name() string {
	return fmt.Sprintf("importing cache mount from %s", i.ref)
}

func (i *contentImporter) Import(ctx context.Context, id string, destPath string) error {
	bklog.G(ctx).Debugf("importing cache mount %s to %s from %s", id, destPath, i.ref)

	// Fetch and parse the manifest
	manifest, err := i.fetchManifest(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to fetch manifest")
	}

	// Validate the manifest
	if len(manifest.Layers) == 0 {
		return errors.New("cache mount manifest has no layers")
	}

	// Verify this is the correct cache mount
	if manifestID, ok := manifest.Annotations[AnnotationCacheMountID]; ok {
		if manifestID != id {
			bklog.G(ctx).Warnf("cache mount ID mismatch: expected %s, got %s", id, manifestID)
		}
	}

	// Create destination directory if it doesn't exist
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return errors.Wrapf(err, "failed to create destination directory %s", destPath)
	}

	// Extract each layer
	for idx, layer := range manifest.Layers {
		if err := i.extractLayer(ctx, id, layer, destPath, idx); err != nil {
			return errors.Wrapf(err, "failed to extract layer %d", idx)
		}
	}

	bklog.G(ctx).Debugf("successfully imported cache mount %s to %s", id, destPath)
	return nil
}

func (i *contentImporter) fetchManifest(ctx context.Context) (*ocispecs.Manifest, error) {
	fetchDone := progress.OneOff(ctx, "fetching cache mount manifest")

	ra, err := i.provider.ReaderAt(ctx, i.desc)
	if err != nil {
		fetchDone(err)
		return nil, errors.Wrap(err, "failed to get manifest reader")
	}
	defer ra.Close()

	data := make([]byte, i.desc.Size)
	if _, err := ra.ReadAt(data, 0); err != nil {
		fetchDone(err)
		return nil, errors.Wrap(err, "failed to read manifest")
	}

	var manifest ocispecs.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		// Try parsing as index first (in case it's a manifest list)
		var index ocispecs.Index
		if err2 := json.Unmarshal(data, &index); err2 == nil {
			fetchDone(errors.New("manifest list not supported for cache mount import"))
			return nil, errors.New("cache mount import requires a single manifest, not a manifest list")
		}
		fetchDone(err)
		return nil, errors.Wrap(err, "failed to parse manifest")
	}

	fetchDone(nil)
	return &manifest, nil
}

func (i *contentImporter) extractLayer(ctx context.Context, id string, desc ocispecs.Descriptor, destPath string, idx int) error {
	extractDone := progress.OneOff(ctx, fmt.Sprintf("extracting cache mount layer %d for %s", idx, id))

	ra, err := i.provider.ReaderAt(ctx, desc)
	if err != nil {
		extractDone(err)
		return errors.Wrap(err, "failed to get layer reader")
	}
	defer ra.Close()

	// Determine if the layer is compressed
	var reader io.Reader = content.NewReader(ra)

	switch desc.MediaType {
	case ocispecs.MediaTypeImageLayerGzip, images.MediaTypeDockerSchema2LayerGzip:
		gr, err := gzip.NewReader(reader)
		if err != nil {
			extractDone(err)
			return errors.Wrap(err, "failed to create gzip reader")
		}
		defer gr.Close()
		reader = gr
	case ocispecs.MediaTypeImageLayer, images.MediaTypeDockerSchema2Layer:
		// Uncompressed, use as-is
	default:
		bklog.G(ctx).Warnf("unknown layer media type %s, attempting gzip decompression", desc.MediaType)
		gr, err := gzip.NewReader(reader)
		if err != nil {
			// Fall back to uncompressed
			ra2, _ := i.provider.ReaderAt(ctx, desc)
			defer ra2.Close()
			reader = content.NewReader(ra2)
		} else {
			defer gr.Close()
			reader = gr
		}
	}

	// Extract tar archive
	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			extractDone(err)
			return errors.Wrap(err, "failed to read tar header")
		}

		// Sanitize the path to prevent directory traversal
		targetPath := filepath.Join(destPath, filepath.Clean(header.Name))
		if !isSubPath(destPath, targetPath) {
			bklog.G(ctx).Warnf("skipping potentially unsafe path: %s", header.Name)
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create directory %s", targetPath)
			}

		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create parent directory for %s", targetPath)
			}

			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create file %s", targetPath)
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				extractDone(err)
				return errors.Wrapf(err, "failed to write file %s", targetPath)
			}
			f.Close()

		case tar.TypeSymlink:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create parent directory for symlink %s", targetPath)
			}

			// Remove existing file/symlink if it exists
			os.Remove(targetPath)

			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create symlink %s", targetPath)
			}

		case tar.TypeLink:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create parent directory for hard link %s", targetPath)
			}

			linkTarget := filepath.Join(destPath, filepath.Clean(header.Linkname))
			if !isSubPath(destPath, linkTarget) {
				bklog.G(ctx).Warnf("skipping potentially unsafe hard link target: %s", header.Linkname)
				continue
			}

			// Remove existing file if it exists
			os.Remove(targetPath)

			if err := os.Link(linkTarget, targetPath); err != nil {
				extractDone(err)
				return errors.Wrapf(err, "failed to create hard link %s", targetPath)
			}

		default:
			bklog.G(ctx).Debugf("skipping unsupported tar entry type %d for %s", header.Typeflag, header.Name)
		}

		// Set modification time
		if err := os.Chtimes(targetPath, header.AccessTime, header.ModTime); err != nil {
			// Non-fatal, just log
			bklog.G(ctx).Debugf("failed to set times for %s: %v", targetPath, err)
		}
	}

	extractDone(nil)
	return nil
}

// isSubPath checks if target is a subdirectory of base
// It allows dotfiles (like .npmrc, .cache) but blocks directory traversal (..)
func isSubPath(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	// Check that we don't escape the base directory via ".." traversal
	// Allow dotfiles - only block if path is exactly ".." or starts with "../"
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
