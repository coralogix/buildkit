package cachemount

import (
	"context"
	"strconv"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/session"
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

		scope, hosts := registryConfig(hosts, ref, resolver.ScopeType{}, insecure)
		remote := resolver.DefaultPool.GetResolver(hosts, refString, scope, sm, g)

		xref, desc, err := remote.Resolve(ctx, refString)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}

		fetcher, err := remote.Fetcher(ctx, xref)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}

		provider := contentutil.FromFetcher(limited.Default.WrapFetcher(fetcher, refString))

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
