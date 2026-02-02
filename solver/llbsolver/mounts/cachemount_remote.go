package mounts

import (
	"context"
	"sync"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/remotecache/cachemount"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/bklog"
	"github.com/pkg/errors"
)

// CacheMountRemoteConfig holds the configuration for remote cache mount operations
type CacheMountRemoteConfig struct {
	// Imports maps cache mount IDs to their import configurations
	Imports map[string]CacheMountImportConfig

	// Exports maps cache mount IDs to their export configurations
	Exports map[string]CacheMountExportConfig
}

// CacheMountImportConfig holds import configuration for a single cache mount
type CacheMountImportConfig struct {
	Type  string
	Attrs map[string]string
}

// CacheMountExportConfig holds export configuration for a single cache mount
type CacheMountExportConfig struct {
	Type  string
	Attrs map[string]string
}

// CacheMountRemoteManager manages remote import/export of cache mounts
type CacheMountRemoteManager struct {
	mm *MountManager
	sm *session.Manager

	importResolvers map[string]cachemount.ResolveCacheMountImporterFunc
	exportResolvers map[string]cachemount.ResolveCacheMountExporterFunc

	// Track which cache mounts have been imported/exported
	importedMu sync.Mutex
	imported   map[string]bool

	exportedMu sync.Mutex
	exported   map[string]bool

	config *CacheMountRemoteConfig
}

// NewCacheMountRemoteManager creates a new cache mount remote manager
func NewCacheMountRemoteManager(mm *MountManager, sm *session.Manager) *CacheMountRemoteManager {
	return &CacheMountRemoteManager{
		mm:              mm,
		sm:              sm,
		importResolvers: make(map[string]cachemount.ResolveCacheMountImporterFunc),
		exportResolvers: make(map[string]cachemount.ResolveCacheMountExporterFunc),
		imported:        make(map[string]bool),
		exported:        make(map[string]bool),
	}
}

// RegisterImportResolver registers an importer resolver for a specific type
func (m *CacheMountRemoteManager) RegisterImportResolver(typ string, fn cachemount.ResolveCacheMountImporterFunc) {
	m.importResolvers[typ] = fn
}

// RegisterExportResolver registers an exporter resolver for a specific type
func (m *CacheMountRemoteManager) RegisterExportResolver(typ string, fn cachemount.ResolveCacheMountExporterFunc) {
	m.exportResolvers[typ] = fn
}

// SetConfig sets the import/export configuration
func (m *CacheMountRemoteManager) SetConfig(config *CacheMountRemoteConfig) {
	m.config = config
}

// ImportCacheMount imports a cache mount from remote storage if configured
// This should be called before the cache mount is first used
func (m *CacheMountRemoteManager) ImportCacheMount(ctx context.Context, id string, g session.Group) error {
	if m.config == nil {
		return nil
	}

	importConfig, ok := m.config.Imports[id]
	if !ok {
		return nil // No import configured for this cache mount
	}

	// Check if already imported
	m.importedMu.Lock()
	if m.imported[id] {
		m.importedMu.Unlock()
		return nil
	}
	m.imported[id] = true
	m.importedMu.Unlock()

	resolver, ok := m.importResolvers[importConfig.Type]
	if !ok {
		return errors.Errorf("unknown cache mount import type: %s", importConfig.Type)
	}

	importer, _, err := resolver(ctx, g, importConfig.Attrs)
	if err != nil {
		// Log warning but don't fail - cache mount can be recreated
		bklog.G(ctx).WithError(err).Warnf("failed to resolve cache mount importer for %s", id)
		return nil
	}

	// Get or create the cache mount ref to get its mount path
	destPath, cleanup, err := m.getCacheMountPath(ctx, id, g)
	if err != nil {
		bklog.G(ctx).WithError(err).Warnf("failed to get cache mount path for import: %s", id)
		return nil
	}
	defer cleanup()

	if err := importer.Import(ctx, id, destPath); err != nil {
		// Log warning but don't fail - cache mount can be recreated
		bklog.G(ctx).WithError(err).Warnf("failed to import cache mount %s", id)
		return nil
	}

	bklog.G(ctx).Infof("successfully imported cache mount %s from %s", id, importConfig.Type)
	return nil
}

// ExportCacheMount exports a cache mount to remote storage if configured
// This should be called after the build completes
func (m *CacheMountRemoteManager) ExportCacheMount(ctx context.Context, id string, g session.Group) error {
	if m.config == nil {
		return nil
	}

	exportConfig, ok := m.config.Exports[id]
	if !ok {
		return nil // No export configured for this cache mount
	}

	// Check if already exported
	m.exportedMu.Lock()
	if m.exported[id] {
		m.exportedMu.Unlock()
		return nil
	}
	m.exported[id] = true
	m.exportedMu.Unlock()

	resolver, ok := m.exportResolvers[exportConfig.Type]
	if !ok {
		return errors.Errorf("unknown cache mount export type: %s", exportConfig.Type)
	}

	exporter, err := resolver(ctx, g, exportConfig.Attrs)
	if err != nil {
		return errors.Wrapf(err, "failed to resolve cache mount exporter for %s", id)
	}

	// Get the cache mount path
	sourcePath, cleanup, err := m.getCacheMountPath(ctx, id, g)
	if err != nil {
		return errors.Wrapf(err, "failed to get cache mount path for export: %s", id)
	}
	defer cleanup()

	if _, err := exporter.Export(ctx, id, sourcePath); err != nil {
		return errors.Wrapf(err, "failed to export cache mount %s", id)
	}

	if _, err := exporter.Finalize(ctx); err != nil {
		return errors.Wrapf(err, "failed to finalize cache mount export %s", id)
	}

	bklog.G(ctx).Infof("successfully exported cache mount %s to %s", id, exportConfig.Type)
	return nil
}

// ExportAllCacheMounts exports all configured cache mounts
func (m *CacheMountRemoteManager) ExportAllCacheMounts(ctx context.Context, g session.Group) error {
	if m.config == nil {
		return nil
	}

	var errs []error
	for id := range m.config.Exports {
		if err := m.ExportCacheMount(ctx, id, g); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Errorf("failed to export %d cache mounts", len(errs))
	}
	return nil
}

// getCacheMountPath gets the filesystem path for a cache mount
func (m *CacheMountRemoteManager) getCacheMountPath(ctx context.Context, id string, g session.Group) (string, func(), error) {
	// Search for existing cache mount
	refs, err := SearchCacheDir(ctx, m.mm.cm, id, false)
	if err != nil {
		return "", nil, err
	}

	var mref cache.MutableRef
	if len(refs) > 0 {
		// Try to get existing ref
		mref, err = m.mm.cm.GetMutable(ctx, refs[0].ID())
		if err != nil && !errors.Is(err, cache.ErrLocked) {
			return "", nil, err
		}
	}

	if mref == nil {
		// Create new ref for import
		mref, err = m.mm.cm.New(ctx, nil, g,
			cache.WithRecordType("exec.cachemount"),
			cache.WithDescription("cache mount "+id),
			cache.CachePolicyRetain,
		)
		if err != nil {
			return "", nil, err
		}

		// Set the cache dir index
		md := CacheRefMetadata{mref}
		if err := md.setCacheDirIndex(id); err != nil {
			mref.Release(context.TODO())
			return "", nil, err
		}
	}

	// Mount the ref
	mnt, err := mref.Mount(ctx, false, g)
	if err != nil {
		mref.Release(context.TODO())
		return "", nil, err
	}

	lm := snapshot.LocalMounter(mnt)
	mountPath, err := lm.Mount()
	if err != nil {
		mref.Release(context.TODO())
		return "", nil, err
	}

	cleanup := func() {
		lm.Unmount()
		mref.Release(context.TODO())
	}

	return mountPath, cleanup, nil
}

// GetUsedCacheMountIDs returns the IDs of cache mounts that have been accessed
func (m *CacheMountRemoteManager) GetUsedCacheMountIDs() []string {
	m.mm.cacheMountsMu.Lock()
	defer m.mm.cacheMountsMu.Unlock()

	ids := make([]string, 0, len(m.mm.cacheMounts))
	for key := range m.mm.cacheMounts {
		// Extract the ID from the key (format is "id" or "id:refID")
		id := key
		if idx := len(key); idx > 0 {
			for i, c := range key {
				if c == ':' {
					id = key[:i]
					break
				}
			}
		}
		ids = append(ids, id)
	}
	return ids
}
