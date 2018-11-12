package tfx

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/dag"
	tf "github.com/hashicorp/terraform/terraform"
)

var (
	walkApplyOnce sync.Once
	walkApply     tf.Interpolater
)

// patch performs an apply operation without a config and returns the new state.
// ResourceProvider.Apply() only requires state and diff. Regular apply uses the
// config to fill in some blanks, such as the lifecycle info, and to validate
// the diff, which we don't want to do. So while the graph and evaluation have
// to be modified, the core idea here is perfectly safe and (mostly) hack-free.
func patch(opts *tf.ContextOpts) (*tf.State, error) {
	if opts.Destroy {
		// Need walkDestroy to implement this
		panic("tfx: patch does not support pure destroy operations")
	}

	// Create context with a copy of the original state
	orig, state := opts.State, opts.State.DeepCopy()
	opts.State = state
	c, err := tf.NewContext(opts)
	if opts.State = orig; err != nil {
		return nil, err
	}

	// HACK: Get contextComponentFactory
	comps := (&tf.ContextGraphWalker{Context: c}).
		EnterPath(tf.RootModulePath).(*tf.BuiltinEvalContext).Components

	// Build patch graph
	graph, err := (&patchGraphBuilder{tf.ApplyGraphBuilder{
		Diff:         opts.Diff,
		State:        state,
		Providers:    comps.ResourceProviders(),
		Provisioners: comps.ResourceProvisioners(),
		Targets:      opts.Targets,
		Destroy:      opts.Destroy,
		Validate:     true,
	}}).Build(tf.RootModulePath)
	if err != nil {
		return nil, err
	}

	// HACK: Get walkApply value
	walkApplyOnce.Do(func() {
		var c tf.Context // Avoid deep copy of the real state
		walkApply.Operation = c.Interpolater().Operation
	})

	// Walk the graph
	w := &patchGraphWalker{ContextGraphWalker: tf.ContextGraphWalker{
		Context:     c,
		Operation:   walkApply.Operation,
		StopContext: context.Background(),
	}}
	if err = graph.Walk(w); len(w.ValidationErrors) > 0 {
		err = multierror.Append(err, w.ValidationErrors...)
	}

	// Stop providers and provisioners
	for _, p := range w.rootCtx.ProviderCache {
		p.Stop()
	}
	for _, p := range w.rootCtx.ProvisionerCache {
		p.Stop()
	}
	return state, err
}

// patchGraphWalker intercepts EnterPath calls to save a reference to the root
// EvalContext, which exposes ContextGraphWalker state.
type patchGraphWalker struct {
	tf.ContextGraphWalker
	rootCtx *tf.BuiltinEvalContext
}

func (w *patchGraphWalker) EnterPath(path []string) tf.EvalContext {
	ctx := w.ContextGraphWalker.EnterPath(path)
	if w.rootCtx == nil {
		w.rootCtx = ctx.(*tf.BuiltinEvalContext)
	}
	return ctx
}

// patchGraphBuilder is a config-free ApplyGraphBuilder.
type patchGraphBuilder struct{ tf.ApplyGraphBuilder }

func (b *patchGraphBuilder) Build(path []string) (*tf.Graph, error) {
	return (&tf.BasicGraphBuilder{
		Steps:    b.Steps(),
		Validate: b.Validate,
		Name:     "PatchGraphBuilder",
	}).Build(path)
}

func (b *patchGraphBuilder) Steps() []tf.GraphTransformer {
	concreteResource := func(a *tf.NodeAbstractResource) dag.Vertex {
		return &nodePatchableResource{tf.NodeApplyableResource{
			NodeAbstractResource: a,
		}}
	}
	steps := b.ApplyGraphBuilder.Steps()
	multi := reflect.TypeOf(tf.GraphTransformMulti())

	// Filter transformers, keeping only those that do not require a config
	keep := steps[:0]
	for _, t := range steps {
		switch t := t.(type) {
		case *tf.DiffTransformer:
			// Replace NodeApplyableResource with nodePatchableResource
			t.Concrete = concreteResource

		case *tf.AttachStateTransformer,
			*tf.DestroyEdgeTransformer,
			*tf.CBDEdgeTransformer,
			*tf.MissingProvisionerTransformer,
			*tf.ProvisionerTransformer,
			*tf.ReferenceTransformer,
			*tf.CountBoundaryTransformer,
			*tf.TargetsTransformer,
			*tf.CloseProviderTransformer,
			*tf.CloseProvisionerTransformer,
			*tf.RootTransformer,
			*tf.TransitiveReductionTransformer:

		case nil,
			*tf.OrphanOutputTransformer,
			*tf.AttachResourceConfigTransformer,
			*tf.RootVariableTransformer,
			*tf.LocalTransformer,
			*tf.OutputTransformer,
			*tf.ModuleVariableTransformer,
			*tf.RemovedModuleTransformer:
			continue

		default:
			if reflect.TypeOf(t) != multi {
				panic(fmt.Sprintf("tfx: unknown GraphTransformer type %T", t))
			}
		}
		keep = append(keep, t)
	}
	return keep
}

// nodePatchableResource is a config-free NodeApplyableResource.
type nodePatchableResource struct{ tf.NodeApplyableResource }

func (n *nodePatchableResource) EvalTree() tf.EvalNode {
	// NodeApplyableResource.EvalTree() expects a valid Config pointer, so we
	// create a minimal config just for that. RawConfig is only needed for
	// ReferencesFromConfig call.
	raw := new(config.RawConfig)
	n.Config = &config.Resource{
		Mode:      n.Addr.Mode,
		Name:      n.Addr.Name,
		Type:      n.Addr.Type,
		RawCount:  raw,
		RawConfig: raw,
	}
	if n.ResourceState != nil {
		n.Config.Provider = n.ResourceState.Provider
		n.Config.DependsOn = n.ResourceState.Dependencies
	}
	seq := n.NodeApplyableResource.EvalTree().(*tf.EvalSequence)
	n.Config.RawCount = nil
	n.Config.RawConfig = nil

	// Filter nodes, keeping only those that do not require a config
	keep := seq.Nodes[:0]
	for _, e := range seq.Nodes {
		switch e.(type) {
		case *tf.EvalInstanceInfo,
			*tf.EvalReadDiff,
			*tf.EvalIf,
			*tf.EvalGetProvider,
			*tf.EvalReadDataApply,
			*tf.EvalReadState,
			*tf.EvalApplyPre,
			*tf.EvalApply,
			*tf.EvalWriteState,
			*tf.EvalApplyProvisioners,
			*tf.EvalWriteDiff,
			*tf.EvalApplyPost,
			*tf.EvalUpdateStateHook:

		case nil,
			*tf.EvalInterpolate,
			*tf.EvalValidateResource,
			*tf.EvalReadDataDiff,
			*tf.EvalDiff,
			*tf.EvalCompareDiff:
			continue

		default:
			panic(fmt.Sprintf("tfx: unknown EvalNode type %T", e))
		}
		keep = append(keep, e)
	}
	seq.Nodes = keep
	return seq
}
