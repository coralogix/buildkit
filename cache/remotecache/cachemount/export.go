package cachemount

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/progress"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// contentExporter exports cache mount contents as OCI artifacts
type contentExporter struct {
	ingester content.Ingester
	ref      string
	oci      bool

	// exported holds the exported descriptors for finalization
	exported map[string]*ocispecs.Descriptor
}

// NewExporter creates a new cache mount exporter
func NewExporter(ingester content.Ingester, ref string, oci bool) Exporter {
	return &contentExporter{
		ingester: ingester,
		ref:      ref,
		oci:      oci,
		exported: make(map[string]*ocispecs.Descriptor),
	}
}

func (e *contentExporter) Name() string {
	return fmt.Sprintf("exporting cache mount to %s", e.ref)
}

// ErrCacheMountEmpty is returned when a cache mount directory is empty
var ErrCacheMountEmpty = errors.New("cache mount is empty")

func (e *contentExporter) Export(ctx context.Context, id string, sourcePath string) (*ocispecs.Descriptor, error) {
	bklog.G(ctx).Debugf("exporting cache mount %s from %s", id, sourcePath)

	// Check if source exists
	info, err := os.Stat(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.Wrapf(err, "cache mount source %s does not exist", sourcePath)
		}
		return nil, errors.Wrapf(err, "failed to stat cache mount source %s", sourcePath)
	}
	if !info.IsDir() {
		return nil, errors.Errorf("cache mount source %s is not a directory", sourcePath)
	}

	// Check if directory is empty
	empty, err := isDirEmpty(sourcePath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to check if cache mount %s is empty", sourcePath)
	}
	if empty {
		bklog.G(ctx).Debugf("cache mount %s is empty, skipping export", id)
		return nil, ErrCacheMountEmpty
	}

	// Create the layer (tar + gzip of the directory contents)
	layerDesc, originalSize, err := e.createLayer(ctx, id, sourcePath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cache mount layer")
	}

	// Create the config
	configDesc, err := e.createConfig(ctx, id, originalSize)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cache mount config")
	}

	// Create and push the manifest
	manifest := ocispecs.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispecs.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispecs.Descriptor{layerDesc},
		Annotations: map[string]string{
			AnnotationCacheMountID:      id,
			AnnotationCacheMountVersion: CurrentVersion,
			AnnotationCacheMountCreated: time.Now().UTC().Format(time.RFC3339),
		},
	}

	manifestDesc, err := e.pushManifest(ctx, manifest)
	if err != nil {
		return nil, errors.Wrap(err, "failed to push cache mount manifest")
	}

	e.exported[id] = manifestDesc
	return manifestDesc, nil
}

func (e *contentExporter) createLayer(ctx context.Context, id string, sourcePath string) (ocispecs.Descriptor, int64, error) {
	layerDone := progress.OneOff(ctx, fmt.Sprintf("creating cache mount layer for %s", id))

	// Create a temp file to stream the tar to avoid memory issues with large caches
	tmpFile, err := os.CreateTemp("", "cachemount-layer-*.tar")
	if err != nil {
		layerDone(err)
		return ocispecs.Descriptor{}, 0, errors.Wrap(err, "failed to create temp file for layer")
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Use a digest writer to compute digest while writing
	digester := digest.Canonical.Digester()
	multiWriter := io.MultiWriter(tmpFile, digester.Hash())

	tw := tar.NewWriter(multiWriter)

	var originalSize int64

	// Walk the source directory and add files to the tar
	err = filepath.Walk(sourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(sourcePath, path)
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return errors.Wrapf(err, "failed to create tar header for %s", relPath)
		}
		header.Name = relPath

		// Handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return errors.Wrapf(err, "failed to read symlink %s", path)
			}
			header.Linkname = link
		}

		if err := tw.WriteHeader(header); err != nil {
			return errors.Wrapf(err, "failed to write tar header for %s", relPath)
		}

		// Write file contents for regular files
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return errors.Wrapf(err, "failed to open file %s", path)
			}
			defer f.Close()

			n, err := io.Copy(tw, f)
			if err != nil {
				return errors.Wrapf(err, "failed to write file %s to tar", relPath)
			}
			originalSize += n
		}

		return nil
	})

	if err != nil {
		tw.Close()
		tmpFile.Close()
		layerDone(err)
		return ocispecs.Descriptor{}, 0, errors.Wrap(err, "failed to create tar archive")
	}

	if err := tw.Close(); err != nil {
		tmpFile.Close()
		layerDone(err)
		return ocispecs.Descriptor{}, 0, errors.Wrap(err, "failed to close tar writer")
	}

	// Get the file size and digest
	fileInfo, err := tmpFile.Stat()
	if err != nil {
		tmpFile.Close()
		layerDone(err)
		return ocispecs.Descriptor{}, 0, errors.Wrap(err, "failed to stat temp file")
	}
	layerSize := fileInfo.Size()
	dgst := digester.Digest()

	// Close and reopen for reading
	tmpFile.Close()
	tmpFile, err = os.Open(tmpPath)
	if err != nil {
		layerDone(err)
		return ocispecs.Descriptor{}, 0, errors.Wrap(err, "failed to reopen temp file for reading")
	}
	defer tmpFile.Close()

	desc := ocispecs.Descriptor{
		MediaType: ocispecs.MediaTypeImageLayer,
		Digest:    dgst,
		Size:      layerSize,
		Annotations: map[string]string{
			AnnotationCacheMountID: id,
		},
	}

	if err := content.WriteBlob(ctx, e.ingester, dgst.String(), tmpFile, desc); err != nil {
		layerDone(err)
		return ocispecs.Descriptor{}, 0, errors.Wrap(err, "failed to write layer blob")
	}

	layerDone(nil)
	bklog.G(ctx).Debugf("created cache mount layer %s: %d bytes (original: %d bytes)", dgst, layerSize, originalSize)

	return desc, originalSize, nil
}

func (e *contentExporter) createConfig(ctx context.Context, id string, originalSize int64) (ocispecs.Descriptor, error) {
	configDone := progress.OneOff(ctx, fmt.Sprintf("creating cache mount config for %s", id))

	config := Config{
		ID:           id,
		Created:      time.Now().UTC().Format(time.RFC3339),
		OriginalSize: originalSize,
	}

	data, err := json.Marshal(config)
	if err != nil {
		configDone(err)
		return ocispecs.Descriptor{}, errors.Wrap(err, "failed to marshal config")
	}

	dgst := digest.FromBytes(data)
	desc := ocispecs.Descriptor{
		MediaType: MediaTypeCacheMountConfig,
		Digest:    dgst,
		Size:      int64(len(data)),
	}

	if err := content.WriteBlob(ctx, e.ingester, dgst.String(), bytes.NewReader(data), desc); err != nil {
		configDone(err)
		return ocispecs.Descriptor{}, errors.Wrap(err, "failed to write config blob")
	}

	configDone(nil)
	return desc, nil
}

func (e *contentExporter) pushManifest(ctx context.Context, manifest ocispecs.Manifest) (*ocispecs.Descriptor, error) {
	manifestDone := progress.OneOff(ctx, "pushing cache mount manifest")

	data, err := json.Marshal(manifest)
	if err != nil {
		manifestDone(err)
		return nil, errors.Wrap(err, "failed to marshal manifest")
	}

	dgst := digest.FromBytes(data)
	desc := ocispecs.Descriptor{
		MediaType: manifest.MediaType,
		Digest:    dgst,
		Size:      int64(len(data)),
	}

	if err := content.WriteBlob(ctx, e.ingester, dgst.String(), bytes.NewReader(data), desc); err != nil {
		manifestDone(err)
		return nil, errors.Wrap(err, "failed to write manifest blob")
	}

	manifestDone(nil)
	bklog.G(ctx).Debugf("pushed cache mount manifest %s", dgst)

	return &desc, nil
}

func (e *contentExporter) Finalize(ctx context.Context) (map[string]string, error) {
	res := make(map[string]string)

	for id, desc := range e.exported {
		descJSON, err := json.Marshal(desc)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal descriptor for %s", id)
		}
		res[fmt.Sprintf("%s.%s", ExporterResponseCacheMountManifest, id)] = string(descJSON)
		res[fmt.Sprintf("%s.%s", ExporterResponseCacheMountRef, id)] = e.ref
		res[fmt.Sprintf("%s.%s", ExporterResponseCacheMountSize, id)] = fmt.Sprintf("%d", desc.Size)
	}

	return res, nil
}

// isDirEmpty checks if a directory is empty (no files or subdirectories)
func isDirEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	// Read just one entry - if we get io.EOF, the directory is empty
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}
