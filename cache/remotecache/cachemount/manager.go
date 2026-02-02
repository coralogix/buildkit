package cachemount

import (
	"context"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/bklog"
	"github.com/pkg/errors"
)

// ImportEntry represents a cache mount import configuration
type ImportEntry struct {
	ID    string
	Type  string
	Attrs map[string]string
}

// ExportEntry represents a cache mount export configuration
type ExportEntry struct {
	ID    string
	Type  string
	Attrs map[string]string
}

// ExportResult contains the result of a cache mount export
type ExportResult struct {
	ID     string
	Ref    string
	Digest string
}

// NewImportEntry creates a new ImportEntry
func NewImportEntry(id, typ string, attrs map[string]string) *ImportEntry {
	return &ImportEntry{
		ID:    id,
		Type:  typ,
		Attrs: attrs,
	}
}

// NewExportEntry creates a new ExportEntry
func NewExportEntry(id, typ string, attrs map[string]string) *ExportEntry {
	return &ExportEntry{
		ID:    id,
		Type:  typ,
		Attrs: attrs,
	}
}

// Manager manages cache mount imports and exports
type Manager struct {
	sm    *session.Manager
	hosts docker.RegistryHosts
	cs    content.Store

	importsMu      sync.Mutex
	pendingImports map[string]*ImportEntry

	exportsMu      sync.Mutex
	pendingExports map[string]*ExportEntry

	completedMu      sync.Mutex
	completedImports map[string]bool
}

// NewManager creates a new cache mount manager
func NewManager(sm *session.Manager, hosts docker.RegistryHosts, cs content.Store) *Manager {
	return &Manager{
		sm:               sm,
		hosts:            hosts,
		cs:               cs,
		pendingImports:   make(map[string]*ImportEntry),
		pendingExports:   make(map[string]*ExportEntry),
		completedImports: make(map[string]bool),
	}
}

// AddImport adds a cache mount import configuration
func (m *Manager) AddImport(entry *ImportEntry) {
	m.importsMu.Lock()
	defer m.importsMu.Unlock()
	m.pendingImports[entry.ID] = entry
}

// AddExport adds a cache mount export configuration
func (m *Manager) AddExport(entry *ExportEntry) {
	m.exportsMu.Lock()
	defer m.exportsMu.Unlock()
	m.pendingExports[entry.ID] = entry
}

// HasImport checks if an import is configured for the given ID
func (m *Manager) HasImport(id string) bool {
	m.importsMu.Lock()
	defer m.importsMu.Unlock()
	_, ok := m.pendingImports[id]
	return ok
}

// HasExport checks if an export is configured for the given ID
func (m *Manager) HasExport(id string) bool {
	m.exportsMu.Lock()
	defer m.exportsMu.Unlock()
	_, ok := m.pendingExports[id]
	return ok
}

// ClearPending clears all pending imports and exports
func (m *Manager) ClearPending() {
	m.importsMu.Lock()
	m.pendingImports = make(map[string]*ImportEntry)
	m.importsMu.Unlock()

	m.exportsMu.Lock()
	m.pendingExports = make(map[string]*ExportEntry)
	m.exportsMu.Unlock()

	m.completedMu.Lock()
	m.completedImports = make(map[string]bool)
	m.completedMu.Unlock()
}

// Cache mount metadata key for indexing
const (
	keyCacheDir   = "cache-dir"
	cacheDirIndex = keyCacheDir + ":"
)

// TryImport attempts to import a cache mount if configured
func (m *Manager) TryImport(ctx context.Context, cm cache.Manager, id string, g session.Group) (bool, error) {
	// Check if already imported
	m.completedMu.Lock()
	if m.completedImports[id] {
		m.completedMu.Unlock()
		return true, nil
	}
	m.completedMu.Unlock()

	// Check if import is configured
	m.importsMu.Lock()
	entry, ok := m.pendingImports[id]
	m.importsMu.Unlock()

	if !ok {
		return false, nil
	}

	// Perform the import
	if err := m.performImport(ctx, cm, entry, g); err != nil {
		return false, err
	}

	// Mark as completed
	m.completedMu.Lock()
	m.completedImports[id] = true
	m.completedMu.Unlock()

	return true, nil
}

func (m *Manager) performImport(ctx context.Context, cm cache.Manager, entry *ImportEntry, g session.Group) error {
	switch entry.Type {
	case "registry":
		importerFunc := RegistryCacheMountImporterFunc(m.sm, m.cs, m.hosts)
		importer, _, err := importerFunc(ctx, g, entry.Attrs)
		if err != nil {
			return errors.Wrapf(err, "failed to create registry importer for %s", entry.ID)
		}

		// Get or create the cache mount ref (createIfMissing=true for imports)
		destPath, cleanup, err := m.getCacheMountPath(ctx, cm, entry.ID, g, true)
		if err != nil {
			return errors.Wrapf(err, "failed to get cache mount path for import: %s", entry.ID)
		}
		defer cleanup()

		// Perform the import
		if err := importer.Import(ctx, entry.ID, destPath); err != nil {
			return errors.Wrapf(err, "failed to import cache mount %s", entry.ID)
		}

		bklog.G(ctx).Infof("successfully imported cache mount %s from registry", entry.ID)
		return nil
	default:
		return errors.Errorf("unsupported cache mount import type: %s", entry.Type)
	}
}

// RunExports performs all pending exports
func (m *Manager) RunExports(ctx context.Context, cm cache.Manager, g session.Group) (map[string]*ExportResult, error) {
	m.exportsMu.Lock()
	exports := make(map[string]*ExportEntry, len(m.pendingExports))
	for k, v := range m.pendingExports {
		exports[k] = v
	}
	m.exportsMu.Unlock()

	results := make(map[string]*ExportResult)

	for id, entry := range exports {
		result, err := m.performExport(ctx, cm, entry, g)
		if err != nil {
			bklog.G(ctx).WithError(err).Warnf("failed to export cache mount %s", id)
			continue
		}
		results[id] = result
	}

	return results, nil
}

// ErrCacheMountNotFound is returned when a cache mount doesn't exist and cannot be exported
var ErrCacheMountNotFound = errors.New("cache mount not found")

func (m *Manager) performExport(ctx context.Context, cm cache.Manager, entry *ExportEntry, g session.Group) (*ExportResult, error) {
	switch entry.Type {
	case "registry":
		// Get the cache mount path (createIfMissing=false to avoid exporting empty caches)
		sourcePath, cleanup, err := m.getCacheMountPath(ctx, cm, entry.ID, g, false)
		if err != nil {
			if errors.Is(err, ErrCacheMountNotFound) {
				bklog.G(ctx).Warnf("skipping export of cache mount %s: not found or never used", entry.ID)
				return nil, nil
			}
			return nil, errors.Wrapf(err, "failed to get cache mount path for export: %s", entry.ID)
		}
		defer cleanup()

		exporterFunc := RegistryCacheMountExporterFunc(m.sm, m.hosts)
		exporter, err := exporterFunc(ctx, g, entry.Attrs)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create registry exporter for %s", entry.ID)
		}

		// Perform the export
		desc, err := exporter.Export(ctx, entry.ID, sourcePath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to export cache mount %s", entry.ID)
		}

		if _, err := exporter.Finalize(ctx); err != nil {
			return nil, errors.Wrapf(err, "failed to finalize cache mount export %s", entry.ID)
		}

		bklog.G(ctx).Infof("successfully exported cache mount %s to registry", entry.ID)
		return &ExportResult{
			ID:     entry.ID,
			Ref:    entry.Attrs["ref"],
			Digest: desc.Digest.String(),
		}, nil
	default:
		return nil, errors.Errorf("unsupported cache mount export type: %s", entry.Type)
	}
}

// getCacheMountPath gets the filesystem path for a cache mount.
// If createIfMissing is true, a new cache mount ref will be created if none exists.
// If createIfMissing is false and the cache mount doesn't exist, ErrCacheMountNotFound is returned.
func (m *Manager) getCacheMountPath(ctx context.Context, cm cache.Manager, id string, g session.Group, createIfMissing bool) (string, func(), error) {
	// Search for existing cache mount
	key := cacheDirIndex + id
	mds, err := cm.Search(ctx, key, false)
	if err != nil {
		return "", nil, err
	}

	var mref cache.MutableRef
	if len(mds) > 0 {
		// Try to get existing ref
		mref, err = cm.GetMutable(ctx, mds[0].ID())
		if err != nil && !errors.Is(err, cache.ErrLocked) {
			return "", nil, err
		}
		// If locked and we're not creating, return not found
		if mref == nil && !createIfMissing {
			return "", nil, errors.Wrapf(ErrCacheMountNotFound, "cache mount %s is locked", id)
		}
	}

	if mref == nil {
		if !createIfMissing {
			return "", nil, errors.Wrapf(ErrCacheMountNotFound, "cache mount %s does not exist", id)
		}

		// Create new ref for import
		mref, err = cm.New(ctx, nil, g,
			cache.WithRecordType("exec.cachemount"),
			cache.WithDescription("cache mount "+id),
			cache.CachePolicyRetain,
		)
		if err != nil {
			return "", nil, err
		}

		// Set the cache dir index so this cache mount can be found later
		if err := mref.SetString(keyCacheDir, id, cacheDirIndex+id); err != nil {
			mref.Release(context.Background())
			return "", nil, err
		}
	}

	// Mount the ref
	mnt, err := mref.Mount(ctx, false, g)
	if err != nil {
		mref.Release(context.Background())
		return "", nil, err
	}

	lm := snapshot.LocalMounter(mnt)
	mountPath, err := lm.Mount()
	if err != nil {
		mref.Release(context.Background())
		return "", nil, err
	}

	cleanup := func() {
		lm.Unmount()
		mref.Release(context.Background())
	}

	return mountPath, cleanup, nil
}
