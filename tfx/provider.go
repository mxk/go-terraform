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

// Providers is the default provider registry.
var Providers ProviderReg

// MakeFactory adds a nil error return to a standard provider constructor to
// match factory function signature.
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

// ProviderReg is an in-memory provider registry. It returns provider resolvers
// for Terraform context operations.
type ProviderReg struct {
	reg            map[string]*provider
	schemaResolver tf.ResourceProviderResolverFunc
}

// Register adds a new provider factory to the registry. If the provider is
// implemented via schema.Provider, then f must return a new instance for each
// call (i.e. do not use terraform.ResourceProviderFactoryFixed wrapper).
func (r *ProviderReg) Register(name, version string, f tf.ResourceProviderFactory) *ProviderReg {
	if r.reg == nil {
		r.reg = make(map[string]*provider)
	} else if _, dup := r.reg[name]; dup {
		panic("tfx: provider already registered: " + name)
	}
	r.reg[name] = &provider{factory: f, version: version}
	return r
}

// Schema returns the schema for the specified provider. It returns nil if the
// provider is not registered or not implemented via schema.Provider. The
// returned value is cached and must only be used for local schema operations.
func (r *ProviderReg) Schema(provider string) *schema.Provider {
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
func (r *ProviderReg) ResourceSchema(typ string) (*schema.Provider, *schema.Resource) {
	if p := r.Schema(config.ResourceProviderFullName(typ, "")); p != nil {
		return p, p.ResourcesMap[typ]
	}
	return nil, nil
}

// NewResource returns a skeleton resource state for the specified resource type
// and ID. The returned string is a normalized resource state key.
func (r *ProviderReg) NewResource(typ, id string) (string, *tf.ResourceState, error) {
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

// ResolveProviders implements terraform.ResourceProviderResolver.
func (r *ProviderReg) ResolveProviders(reqd discovery.PluginRequirements) (
	map[string]tf.ResourceProviderFactory, []error,
) {
	return r.resolve(reqd, false)
}

// SchemaResolver returns a provider resolver containing schema-only providers.
// Configuration and CRUD functions are replaced by no-ops, preventing the
// provider from making any API calls.
func (r *ProviderReg) SchemaResolver() tf.ResourceProviderResolver {
	if r.schemaResolver == nil {
		r.schemaResolver = func(reqd discovery.PluginRequirements) (
			map[string]tf.ResourceProviderFactory, []error,
		) {
			return r.resolve(reqd, true)
		}
	}
	return r.schemaResolver
}

// get returns the provider with the specified name.
func (r *ProviderReg) get(name string) *provider {
	p := r.reg[name]
	if p != nil {
		p.init()
	}
	return p
}

// resolve returns providers that match the specified requirements.
func (r *ProviderReg) resolve(reqd discovery.PluginRequirements, schemaOnly bool) (
	m map[string]tf.ResourceProviderFactory, errs []error,
) {
	m = make(map[string]tf.ResourceProviderFactory, len(reqd))
	for name, req := range reqd {
		if p := r.get(name); p == nil {
			err := fmt.Errorf("provider %q is not available", name)
			errs = append(errs, err)
		} else if !req.Versions.Unconstrained() && p.version != "" &&
			!req.Versions.Allows(p.discVer) {
			err := fmt.Errorf("provider %q v%s does not satisfy %q",
				name, p.version, req.Versions)
			errs = append(errs, err)
		} else if schemaOnly {
			m[name] = p.schemaFactory
		} else {
			m[name] = p.factory
		}
	}
	return
}

// provider contains information for a single provider.
type provider struct {
	factory       tf.ResourceProviderFactory
	schemaFactory tf.ResourceProviderFactory
	version       string
	discVer       discovery.Version
	schema        *schema.Provider
	initDone      bool
}

// init performs a one-time provider initialization.
func (p *provider) init() {
	if p.initDone {
		return
	}
	p.initDone = true
	if p.version != "" {
		p.discVer = discovery.VersionStr(p.version).MustParse()
	}
	if s, err := p.factory(); err == nil {
		if s, ok := s.(*schema.Provider); ok {
			disableProvider(s)
			p.schema = s
			p.schemaFactory = func() (tf.ResourceProvider, error) {
				// Deep copy seems to be slower than getting a new instance
				s, err := p.factory()
				if s != nil {
					disableProvider(s.(*schema.Provider))
				}
				return s, err
			}
		}
	}
}

// disableProvider modifies provider p to prevent it from making any API calls.
func disableProvider(p *schema.Provider) {
	for _, r := range p.ResourcesMap {
		disableResource(r)
	}
	for _, r := range p.DataSourcesMap {
		disableResource(r)
	}
	p.ConfigureFunc = nil
}

// disableResource disables CRUD operations for resource r.
func disableResource(r *schema.Resource) {
	for _, s := range r.Schema {
		disableSchema(s)
	}
	r.MigrateState = nil
	r.Create = noopCreate
	r.Read = noop
	r.Update = noop
	r.Delete = noop
	r.Exists = nil
}

// disableSchema disables CRUD operations for element type of s.
func disableSchema(s *schema.Schema) {
	switch e := s.Elem.(type) {
	case *schema.Schema:
		disableSchema(e)
	case *schema.Resource:
		disableResource(e)
	case nil:
	default:
		panic("tfx: unsupported schema elem type")
	}
}

// noopCreate is a passthrough resource Create function.
func noopCreate(r *schema.ResourceData, _ interface{}) error {
	r.SetId("<computed>")
	return nil
}

// noop is a passthrough Read/Update/Delete function.
func noop(_ *schema.ResourceData, _ interface{}) error { return nil }
