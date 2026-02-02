// Package cachemount provides OCI-based export and import for cache mounts.
// This allows cache mounts (--mount=type=cache) to be stored in and retrieved from
// OCI registries, enabling cache portability across different build environments.
package cachemount

import (
	"context"

	"github.com/moby/buildkit/session"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// MediaTypeCacheMountConfig is the media type for cache mount configuration
	MediaTypeCacheMountConfig = "application/vnd.buildkit.cachemount.config.v1+json"

	// AnnotationCacheMountID is the annotation key for cache mount ID
	AnnotationCacheMountID = "buildkit.cachemount.id"

	// AnnotationCacheMountCreated is the annotation key for creation timestamp
	AnnotationCacheMountCreated = "buildkit.cachemount.created"

	// AnnotationCacheMountVersion is the annotation key for format version
	AnnotationCacheMountVersion = "buildkit.cachemount.version"

	// CurrentVersion is the current cache mount format version
	CurrentVersion = "1"
)

// Config represents the configuration blob for a cache mount OCI artifact
type Config struct {
	// ID is the cache mount identifier
	ID string `json:"id"`

	// Created is the timestamp when this cache was exported
	Created any `json:"created"` // Can be time.Time or string

	// Sharing is the sharing mode (shared, private, locked)
	Sharing string `json:"sharing,omitempty"`

	// ContentHash is an optional hash of the contents for change detection
	ContentHash any `json:"contentHash,omitempty"` // Can be digest.Digest or string

	// Size is the uncompressed size of the cache contents (alias: OriginalSize)
	Size int64 `json:"size,omitempty"`

	// OriginalSize is the uncompressed size (for backward compatibility)
	OriginalSize int64 `json:"originalSize,omitempty"`

	// FileCount is the number of files in the cache mount
	FileCount int64 `json:"fileCount,omitempty"`
}

// ResolveCacheMountExporterFunc is the function signature for resolving cache mount exporters
type ResolveCacheMountExporterFunc func(ctx context.Context, g session.Group, attrs map[string]string) (Exporter, error)

// ResolveCacheMountImporterFunc is the function signature for resolving cache mount importers
type ResolveCacheMountImporterFunc func(ctx context.Context, g session.Group, attrs map[string]string) (Importer, ocispecs.Descriptor, error)

// Exporter exports cache mount contents to remote storage
type Exporter interface {
	// Name returns a human-readable name for the exporter
	Name() string

	// Export exports the cache mount contents from the given source path
	// Returns the manifest descriptor and any metadata
	Export(ctx context.Context, id string, sourcePath string) (*ocispecs.Descriptor, error)

	// Finalize completes the export and returns response metadata
	Finalize(ctx context.Context) (map[string]string, error)
}

// Importer imports cache mount contents from remote storage
type Importer interface {
	// Name returns a human-readable name for the importer
	Name() string

	// Import imports the cache mount contents to the given destination path
	Import(ctx context.Context, id string, destPath string) error
}

// ExporterResponse keys for cache mount export
const (
	// ExporterResponseCacheMountManifest is the key for the manifest descriptor JSON
	ExporterResponseCacheMountManifest = "cachemount.manifest"

	// ExporterResponseCacheMountRef is the key for the reference string
	ExporterResponseCacheMountRef = "cachemount.ref"

	// ExporterResponseCacheMountSize is the key for the exported size
	ExporterResponseCacheMountSize = "cachemount.size"
)
