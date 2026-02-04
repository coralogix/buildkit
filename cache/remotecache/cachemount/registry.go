package cachemount

import (
	"context"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/push"
	"github.com/moby/buildkit/util/resolver"
	resolverconfig "github.com/moby/buildkit/util/resolver/config"
	"github.com/moby/buildkit/util/resolver/limited"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	attrRef      = "ref"
	attrInsecure = "registry.insecure"
)

// registryExporter wraps the content exporter for registry operations
type registryExporter struct {
	Exporter
}

func (*registryExporter) Name() string {
	return "exporting cache mount to registry"
}

// RegistryCacheMountExporterFunc returns a resolver function for registry-based cache mount export
func RegistryCacheMountExporterFunc(sm *session.Manager, hosts docker.RegistryHosts) ResolveCacheMountExporterFunc {
	return func(ctx context.Context, g session.Group, attrs map[string]string) (Exporter, error) {
		ref, err := canonicalizeRef(attrs[attrRef])
		if err != nil {
			return nil, err
		}
		refString := ref.String()

		insecure := false
		if v, ok := attrs[attrInsecure]; ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse %s", attrInsecure)
			}
			insecure = b
		}

		scope, hosts := registryConfig(hosts, ref, resolver.ScopeType{Push: true}, insecure)
		remote := resolver.DefaultPool.GetResolver(hosts, refString, scope, sm, g)

		// Pre-authenticate with registry by calling Resolve first
		// This triggers the auth flow and caches credentials for subsequent Push operations
		// Note: The ref may not exist yet (404 is expected for new caches), but auth will still be cached
		bklog.G(ctx).Debugf("[cache mount export] pre-authenticating with registry for %s", refString)
		_, _, resolveErr := remote.Resolve(ctx, refString)
		if resolveErr != nil {
			// 404/not found is expected for new cache tags - ignore it
			// Other errors (including auth errors) should be logged but we'll try pushing anyway
			errStr := resolveErr.Error()
			if !strings.Contains(errStr, "not found") &&
				!strings.Contains(errStr, "404") &&
				!strings.Contains(errStr, "manifest unknown") &&
				!strings.Contains(errStr, "MANIFEST_UNKNOWN") {
				bklog.G(ctx).Debugf("[cache mount export] pre-auth resolve returned error (may still push): %v", resolveErr)
			}
		}

		pusher, err := push.Pusher(ctx, remote, refString)
		if err != nil {
			return nil, err
		}

		return &registryExporter{
			Exporter: NewExporter(contentutil.FromPusher(pusher), refString, true),
		}, nil
	}
}

// registryImporter wraps the content importer for registry operations
type registryImporter struct {
	Importer
}

func (*registryImporter) Name() string {
	return "importing cache mount from registry"
}

// ErrCacheNotFound is returned when a cache mount image doesn't exist in the registry
var ErrCacheNotFound = errors.New("cache mount not found in registry")

// RegistryCacheMountImporterFunc returns a resolver function for registry-based cache mount import
func RegistryCacheMountImporterFunc(sm *session.Manager, cs content.Store, hosts docker.RegistryHosts) ResolveCacheMountImporterFunc {
	return func(ctx context.Context, g session.Group, attrs map[string]string) (Importer, ocispecs.Descriptor, error) {
		ref, err := canonicalizeRef(attrs[attrRef])
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}
		refString := ref.String()

		insecure := false
		if v, ok := attrs[attrInsecure]; ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, ocispecs.Descriptor{}, errors.Wrapf(err, "failed to parse %s", attrInsecure)
			}
			insecure = b
		}

		bklog.G(ctx).Debugf("[cache mount import] resolving %s (insecure=%v)", refString, insecure)

		scope, hosts := registryConfig(hosts, ref, resolver.ScopeType{}, insecure)
		remote := resolver.DefaultPool.GetResolver(hosts, refString, scope, sm, g)

		bklog.G(ctx).Debugf("[cache mount import] calling Resolve for %s", refString)
		xref, desc, err := remote.Resolve(ctx, refString)
		if err != nil {
			bklog.G(ctx).Debugf("[cache mount import] Resolve failed for %s: %v", refString, err)
			// Check if this is a "not found" error - this is expected for first-time builds
			errStr := err.Error()
			if strings.Contains(errStr, "not found") ||
				strings.Contains(errStr, "404") ||
				strings.Contains(errStr, "manifest unknown") ||
				strings.Contains(errStr, "MANIFEST_UNKNOWN") ||
				strings.Contains(errStr, "NAME_UNKNOWN") {
				return nil, ocispecs.Descriptor{}, errors.Wrapf(ErrCacheNotFound, "ref %s: %v", refString, err)
			}
			return nil, ocispecs.Descriptor{}, err
		}

		bklog.G(ctx).Debugf("[cache mount import] Resolve succeeded for %s, digest=%s", refString, desc.Digest)

		fetcher, err := remote.Fetcher(ctx, xref)
		if err != nil {
			bklog.G(ctx).Debugf("[cache mount import] Fetcher failed for %s: %v", refString, err)
			return nil, ocispecs.Descriptor{}, err
		}

		provider := contentutil.FromFetcher(limited.Default.WrapFetcher(fetcher, refString))

		bklog.G(ctx).Debugf("[cache mount import] importer ready for %s", refString)
		return &registryImporter{
			Importer: NewImporter(provider, desc, refString),
		}, desc, nil
	}
}

func canonicalizeRef(rawRef string) (reference.Named, error) {
	if rawRef == "" {
		return nil, errors.New("missing ref")
	}
	parsed, err := reference.ParseNormalizedNamed(rawRef)
	if err != nil {
		return nil, err
	}
	parsed = reference.TagNameOnly(parsed)
	return parsed, nil
}

func registryConfig(hosts docker.RegistryHosts, ref reference.Named, scope resolver.ScopeType, insecure bool) (resolver.ScopeType, docker.RegistryHosts) {
	if insecure {
		insecureTrue := true
		httpTrue := true
		hosts = resolver.NewRegistryConfig(map[string]resolverconfig.RegistryConfig{
			reference.Domain(ref): {
				Insecure:  &insecureTrue,
				PlainHTTP: &httpTrue,
			},
		})
		scope.Insecure = true
	}
	return scope, hosts
}
