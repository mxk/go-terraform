package tfx

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/plugin/discovery"
	tf "github.com/hashicorp/terraform/terraform"
)

// Providers is the default in-memory provider registry.
var Providers ProviderMap

// MakeFactory adds a nil error return to a standard provider constructor to
// match factory function signature. This should be used instead of
// terraform.ResourceProviderFactoryFixed.
func MakeFactory(f func() tf.ResourceProvider) tf.ResourceProviderFactory {
	return func() (tf.ResourceProvider, error) { return f(), nil }
}

// ProviderVersion extracts the package version from the import path of a
// provider function. This assumes the use of Go modules.
func ProviderVersion(f func() tf.ResourceProvider) string {
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		panic("tfx: provider function not found")
	}
	path, _ := fn.FileLine(fn.Entry())
	for prev := ""; path != prev; prev, path = path, filepath.Dir(path) {
		name := filepath.Base(path)
		if i := strings.Index(name, "@v"); i > 0 {
			return name[i+2:]
		}
	}
	return ""
}

// Config creates schema.ResourceData from a raw config.
func Config(s map[string]*schema.Schema, raw map[string]interface{}) (*schema.ResourceData, error) {
	r, err := config.NewRawConfig(raw)
	if err != nil {
		return nil, err
	}
	b := schema.Backend{Schema: s}
	if err = b.Configure(tf.NewResourceConfig(r)); err != nil {
		return nil, err
	}
	return b.Config(), nil
}

// ProviderMap is an in-memory provider registry. It returns provider resolvers
// for Terraform context operations and provides access to provider schemas.
type ProviderMap map[string]*provider

// Add adds a new provider to the registry. Version is optional. The factory
// function must return a new provider instance for each call (i.e. do not use
// terraform.ResourceProviderFactoryFixed wrapper).
func (r *ProviderMap) Add(name, version string, f tf.ResourceProviderFactory) {
	if *r == nil {
		*r = make(map[string]*provider)
	} else if _, dup := (*r)[name]; dup {
		panic("tfx: provider already registered: " + name)
	}
	p := &provider{version: version}
	p.factory[defaultMode] = f
	(*r)[name] = p
}

// Schema returns the schema for the specified provider. It returns nil if the
// provider is not registered or not implemented via schema.Provider. The
// returned value is cached and must only be used for local schema operations.
func (r ProviderMap) Schema(provider string) *schema.Provider {
	provider = config.ResourceProviderFullName("", provider)
	if i := strings.IndexByte(provider, '.'); i > 0 {
		provider = provider[:i] // Strip alias
	}
	if p := r.get(provider); p != nil {
		return p.schema
	}
	return nil
}

// ResourceSchema returns the provider and resource schema for the specified
// resource type. It returns (nil, nil) if the type is unknown or not
// implemented via schema.Provider.
func (r ProviderMap) ResourceSchema(typ string) (*schema.Provider, *schema.Resource) {
	if p := r.Schema(config.ResourceProviderFullName(typ, "")); p != nil {
		return p, p.ResourcesMap[typ]
	}
	return nil, nil
}

// NewResource returns a skeleton resource state for the specified resource type
// and ID. The returned string is a normalized resource state key.
func (r ProviderMap) NewResource(typ, id string) (string, *tf.ResourceState, error) {
	_, s := r.ResourceSchema(typ)
	if s == nil {
		return "", nil, fmt.Errorf("tfx: unknown resource type %q", typ)
	}
	var meta map[string]interface{}
	if s.SchemaVersion > 0 {
		meta = map[string]interface{}{
			"schema_version": strconv.Itoa(s.SchemaVersion),
		}
	}
	// TODO: Use importers?
	return typ + "." + makeName(id), &tf.ResourceState{
		Type: typ,
		Primary: &tf.InstanceState{
			ID:         id,
			Attributes: map[string]string{"id": id},
			Meta:       meta,
		},
		Provider: tf.ResolveProviderName(
			config.ResourceProviderFullName(typ, ""), nil),
	}, nil
}

// DefaultResolver returns a resolver for unmodified providers.
func (r ProviderMap) DefaultResolver() tf.ResourceProviderResolver {
	return r.resolver(defaultMode)
}

// SchemaResolver returns a resolver for schema-only providers. Provider
// configuration and CRUD operations are disabled, preventing the provider from
// requiring a valid config or making any API calls.
func (r ProviderMap) SchemaResolver() tf.ResourceProviderResolver {
	return r.resolver(schemaMode)
}

// PassthroughResolver returns a SchemaResolver with all schema validations
// disabled.
func (r ProviderMap) PassthroughResolver() tf.ResourceProviderResolver {
	return r.resolver(passthroughMode)
}

// get returns the provider with the specified name.
func (r ProviderMap) get(name string) *provider {
	p := r[name]
	if p != nil {
		p.init()
	}
	return p
}

// resolver returns a provider resolver with the specified mode of operation.
func (r ProviderMap) resolver(mode providerMode) tf.ResourceProviderResolver {
	return tf.ResourceProviderResolverFunc(func(reqd discovery.PluginRequirements) (
		m map[string]tf.ResourceProviderFactory, errs []error,
	) {
		m = make(map[string]tf.ResourceProviderFactory, len(reqd))
		for name, req := range reqd {
			var err error
			if p := r.get(name); p == nil {
				err = fmt.Errorf("provider %q is not available", name)
			} else if !req.Versions.Unconstrained() && p.version != "" &&
				!req.Versions.Allows(p.discVer) {
				err = fmt.Errorf("provider %q v%s does not satisfy %q",
					name, strings.TrimPrefix(p.version, "v"), req.Versions)
			} else if f := p.factory[mode]; f == nil {
				err = fmt.Errorf("provider %q does not support mode %v",
					name, mode)
			} else {
				m[name] = f
				continue
			}
			errs = append(errs, err)
		}
		return
	})
}

// provider contains information for a single provider.
type provider struct {
	factory  [modeCount]tf.ResourceProviderFactory
	schema   *schema.Provider
	version  string
	discVer  discovery.Version
	initDone bool
}

// init parses provider version and creates additional factory functions.
func (p *provider) init() {
	if p.initDone {
		return
	}
	p.initDone = true
	if p.version != "" {
		p.discVer = discovery.VersionStr(p.version).MustParse()
	}
	if p.schema, _ = p.schemaProvider(schemaMode); p.schema != nil {
		p.factory[schemaMode] = func() (tf.ResourceProvider, error) {
			return p.schemaProvider(schemaMode)
		}
		p.factory[passthroughMode] = func() (tf.ResourceProvider, error) {
			return p.schemaProvider(passthroughMode)
		}
	}
}

// schemaProvider returns a new schema.Provider instance configured for the
// specified mode of operation. It returns (nil, nil) if the provider was not
// implemented via schema.Provider.
func (p *provider) schemaProvider(mode providerMode) (*schema.Provider, error) {
	s, err := p.factory[defaultMode]()
	if err == nil {
		if s, ok := s.(*schema.Provider); ok {
			if s == p.schema {
				// Protection against terraform.ResourceProviderFactoryFixed
				panic("tfx: factory returned same provider instance")
			}
			return mode.apply(s), nil
		}
	}
	return nil, err
}

// providerMode alters provider behavior to support non-standard operations.
type providerMode int

const (
	defaultMode     providerMode = iota // Standard operation
	schemaMode                          // Schema-only (no config or API calls)
	passthroughMode                     // Schema-only and no validation
	modeCount
)

func (m providerMode) apply(p *schema.Provider) *schema.Provider {
	if p == nil || m == defaultMode {
		return p
	}
	for _, r := range p.ResourcesMap {
		m.updateResource(r)
	}
	for _, r := range p.DataSourcesMap {
		m.updateResource(r)
	}
	p.ConfigureFunc = nil
	return p
}

func (m providerMode) updateResource(r *schema.Resource) {
	for _, s := range r.Schema {
		m.updateSchema(s)
	}
	r.MigrateState = nil
	r.Create = noopCreate
	r.Read = noop
	r.Update = noop
	r.Delete = noop
	r.Exists = nil
	if m == passthroughMode {
		r.CustomizeDiff = nil
	}
}

func (m providerMode) updateSchema(s *schema.Schema) {
	switch e := s.Elem.(type) {
	case *schema.Schema:
		m.updateSchema(e)
	case *schema.Resource:
		m.updateResource(e)
	case nil:
	default:
		panic("tfx: unsupported schema elem type")
	}
	if m == passthroughMode {
		s.ValidateFunc = nil
	}
}

func noopCreate(r *schema.ResourceData, _ interface{}) error {
	r.SetId("?")
	return nil
}

func noop(_ *schema.ResourceData, _ interface{}) error { return nil }
