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

// InitSchemaProvider should be called from factory functions to initialize new
// schema.Provider instances. It disables DefaultFuncs to ensure deterministic
// behavior (these are normally used to get environment variables), and sets
// zero-value defaults to prevent any user prompts.
func InitSchemaProvider(rp tf.ResourceProvider) *schema.Provider {
	p, ok := rp.(*schema.Provider)
	if !ok {
		panic("tfx: provider not implemented via schema.Provider")
	}
	for _, s := range p.Schema {
		if s.DefaultFunc = nil; s.Default != nil {
			continue
		}
		if v := s.ZeroValue(); !s.Required {
			s.Default = v
		} else {
			s.DefaultFunc = func() (interface{}, error) { return v, nil }
		}
	}
	return p
}

// ProviderMap is an in-memory provider registry. It returns provider resolvers
// for Terraform context operations and provides access to provider schemas.
type ProviderMap map[string]*provider

// Add adds a new provider to the registry. Version is optional. The factory
// function must return a new provider instance for each call (i.e. do not use
// terraform.ResourceProviderFactoryFixed wrapper).
func (pm *ProviderMap) Add(name, version string, f tf.ResourceProviderFactory) {
	if *pm == nil {
		*pm = make(map[string]*provider)
	} else if _, dup := (*pm)[name]; dup {
		panic("tfx: provider already registered: " + name)
	}
	p := &provider{version: version}
	p.factory[defaultMode] = f
	(*pm)[name] = p
}

// Schema returns the schema for the specified provider. It returns nil if the
// provider is not registered or not implemented via schema.Provider. The
// returned value is cached and must only be used for local schema operations.
func (pm ProviderMap) Schema(provider string) *schema.Provider {
	provider = config.ResourceProviderFullName("", provider)
	if i := strings.IndexByte(provider, '.'); i > 0 {
		provider = provider[:i] // Strip alias
	}
	if p := pm.get(provider); p != nil {
		return p.schema
	}
	return nil
}

// ResourceSchema returns the provider and resource schema for the specified
// resource type. It returns (nil, nil) if the type is unknown or not
// implemented via schema.Provider.
func (pm ProviderMap) ResourceSchema(typ string) (*schema.Provider, *schema.Resource) {
	if p := pm.Schema(config.ResourceProviderFullName(typ, "")); p != nil {
		return p, p.ResourcesMap[typ]
	}
	return nil, nil
}

// Resource associates a state key with tf.ResourceState.
type Resource struct {
	*tf.ResourceState
	Key string
}

// NewResource returns a skeleton resource state for the specified resource type
// and ID. If useImport is true, the resource importer is applied to the new
// resource. Importers that return multiple new states or make API calls are not
// supported.
func (pm ProviderMap) NewResource(typ, id string, useImport bool) (Resource, error) {
	_, s := pm.ResourceSchema(typ)
	if s == nil {
		return Resource{}, fmt.Errorf("tfx: unknown resource type %q", typ)
	}
	if id == "" {
		return Resource{}, fmt.Errorf("tfx: empty id for resource type %q", typ)
	}
	var meta map[string]interface{}
	if s.SchemaVersion > 0 {
		meta = map[string]interface{}{
			"schema_version": strconv.Itoa(s.SchemaVersion),
		}
	}
	rs := Resource{
		ResourceState: &tf.ResourceState{
			Type: typ,
			Primary: &tf.InstanceState{
				ID:         id,
				Attributes: map[string]string{"id": id},
				Meta:       meta,
			},
			Provider: tf.ResolveProviderName(
				config.ResourceProviderFullName(typ, ""), nil),
		},
		Key: typ + "." + makeName(id),
	}
	if useImport {
		d, err := s.Importer.State(s.Data(rs.Primary), nil)
		if err != nil {
			return Resource{}, err
		}
		if len(d) != 1 {
			panic(fmt.Sprintf("tfx: %q importer returned %d values",
				typ, len(d)))
		}
		rs.Primary = d[0].State()
	}
	return rs, nil
}

// AttrGen is a attribute value generator used to create resources. Valid value
// types are: string, []string, and func(i int) string. The latter must return
// values for i in the range [0,n). Use "#" key to specify n when there are no
// []string attributes.
type AttrGen map[string]interface{}

// MakeResources calls NewResource for each "id" attribute (or for "#"
// invocations of its generator function) and populates any remaining attribute
// values.
func (pm ProviderMap) MakeResources(typ string, attrs AttrGen) ([]Resource, error) {
	return pm.makeResources(typ, attrs, false)
}

// ImportResources calls NewResource for each "id" attribute (or for "#"
// invocations of its generator function), applies the resource importer, and
// populates any remaining attribute values.
func (pm ProviderMap) ImportResources(typ string, attrs AttrGen) ([]Resource, error) {
	return pm.makeResources(typ, attrs, true)
}

// DefaultResolver returns a resolver for unmodified providers.
func (pm ProviderMap) DefaultResolver() tf.ResourceProviderResolver {
	return pm.resolver(defaultMode)
}

// SchemaResolver returns a resolver for schema-only providers. Provider
// configuration and CRUD operations are disabled, preventing the provider from
// requiring a valid config or making any API calls.
func (pm ProviderMap) SchemaResolver() tf.ResourceProviderResolver {
	return pm.resolver(schemaMode)
}

// PassthroughResolver returns a SchemaResolver with all schema validations
// disabled.
func (pm ProviderMap) PassthroughResolver() tf.ResourceProviderResolver {
	return pm.resolver(passthroughMode)
}

// get returns the provider with the specified name.
func (pm ProviderMap) get(name string) *provider {
	p := pm[name]
	if p != nil {
		p.init()
	}
	return p
}

// resolver returns a provider resolver with the specified mode of operation.
func (pm ProviderMap) resolver(mode providerMode) tf.ResourceProviderResolver {
	return tf.ResourceProviderResolverFunc(func(reqd discovery.PluginRequirements) (
		m map[string]tf.ResourceProviderFactory, errs []error,
	) {
		m = make(map[string]tf.ResourceProviderFactory, len(reqd))
		for name, req := range reqd {
			var err error
			if p := pm.get(name); p == nil {
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

// makeResources implements MakeResources and ImportResources.
func (pm ProviderMap) makeResources(typ string, attrs AttrGen, useImport bool) ([]Resource, error) {
	// Generate IDs
	var ids []string
	switch v := attrs["id"].(type) {
	case string:
		ids = []string{v}
	case []string:
		ids = v
	case func(int) string:
		n := attrs["#"]
		if n == nil {
			for _, v := range attrs {
				if v, ok := v.([]string); ok {
					n = len(v)
					break
				}
			}
			if n == nil {
				panic("tfx: '#' value required for 'id' function")
			}
		}
		ids = make([]string, n.(int))
		for i := range ids {
			ids[i] = v(i)
		}
	default:
		panic("tfx: invalid 'id' attribute value type")
	}

	// Make sure all []string values have identical lengths
	for k, v := range attrs {
		if v, ok := v.([]string); ok && len(v) != len(ids) {
			panic(fmt.Sprintf(
				"tfx: invalid number of %q attributes (have %d, want %d)",
				k, len(v), len(ids)))
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Create resources
	rs := make([]Resource, len(ids))
	var err error
	for i, id := range ids {
		if rs[i], err = pm.NewResource(typ, id, useImport); err != nil {
			return nil, err
		}
	}

	// Set additional attributes
	_, s := pm.ResourceSchema(typ)
	for k, v := range attrs {
		switch k {
		case "#", "id":
			continue
		}
		if s.Schema[k] == nil {
			panic(fmt.Sprintf("tfx: attribute %q not valid for %q", k, typ))
		}
		switch v := v.(type) {
		case string:
			for _, r := range rs {
				r.Primary.Attributes[k] = v
			}
		case []string:
			for i, r := range rs {
				r.Primary.Attributes[k] = v[i]
			}
		case func(int) string:
			for i, r := range rs {
				r.Primary.Attributes[k] = v(i)
			}
		default:
			panic(fmt.Sprintf("tfx: invalid %q attribute value type", k))
		}
	}
	return rs, nil
}

// provider contains information for a single provider.
type provider struct {
	factory  [modeCount]tf.ResourceProviderFactory
	schema   *schema.Provider
	version  string
	discVer  discovery.Version
	initDone bool
}

// Version returns provider version.
func (p *provider) Version() string { return p.version }

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
