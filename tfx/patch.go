package tfx

import (
	"context"
	"fmt"
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
		return &nodePatchableResource{
			NodeApplyableResource: tf.NodeApplyableResource{
				NodeAbstractResource: a,
			},
		}
	}
	steps := b.ApplyGraphBuilder.Steps()
	keep := steps[:0]
	for _, t := range steps {
		switch t := t.(type) {
		case *tf.DiffTransformer:
			// Replace NodeApplyableResource with nodePatchableResource
			t.Concrete = concreteResource

		case *tf.OrphanOutputTransformer,
			*tf.AttachResourceConfigTransformer,
			*tf.RootVariableTransformer,
			*tf.LocalTransformer,
			*tf.OutputTransformer,
			*tf.ModuleVariableTransformer,
			*tf.RemovedModuleTransformer:
			// Filter out transformers that require a valid config
			continue
		}
		keep = append(keep, t)
	}
	return keep
}

// nodePatchableResource is a config-free NodeApplyableResource.
type nodePatchableResource struct {
	tf.NodeApplyableResource
	Config struct{}
}

func (n *nodePatchableResource) EvalTree() tf.EvalNode {
	addr := n.NodeAbstractResource.Addr

	// stateId is the ID to put into the state
	stateKey := tf.ResourceStateKey{
		Name:  addr.Name,
		Type:  addr.Type,
		Mode:  addr.Mode,
		Index: addr.Index,
	}
	stateId := stateKey.String()

	// Build the instance info. More of this will be populated during eval
	info := &tf.InstanceInfo{
		Id:   stateId,
		Type: addr.Type,
	}

	// Build the resource for eval
	resource := &tf.Resource{
		Name:       addr.Name,
		Type:       addr.Type,
		CountIndex: addr.Index,
	}
	if resource.CountIndex < 0 {
		resource.CountIndex = 0
	}

	// Determine the dependencies for the state.
	stateDeps := n.StateReferences()

	// Eval info is different depending on what kind of resource this is
	switch addr.Mode {
	case config.ManagedResourceMode:
		return n.evalTreeManagedResource(stateId, info, resource, stateDeps)
	case config.DataResourceMode:
		return n.evalTreeDataResource(stateId, info, resource, stateDeps)
	default:
		panic(fmt.Errorf("unsupported resource mode %s", addr.Mode))
	}
}

func (n *nodePatchableResource) evalTreeDataResource(
	stateId string, info *tf.InstanceInfo,
	resource *tf.Resource, stateDeps []string) tf.EvalNode {
	var provider tf.ResourceProvider
	//var config *tf.ResourceConfig
	var diff *tf.InstanceDiff
	var state *tf.InstanceState

	return &tf.EvalSequence{
		Nodes: []tf.EvalNode{
			// Build the instance info
			&tf.EvalInstanceInfo{
				Info: info,
			},

			// Get the saved diff for apply
			&tf.EvalReadDiff{
				Name: stateId,
				Diff: &diff,
			},

			// Stop here if we don't actually have a diff
			&tf.EvalIf{
				If: func(ctx tf.EvalContext) (bool, error) {
					if diff == nil {
						return true, tf.EvalEarlyExitError{}
					}

					if diff.GetAttributesLen() == 0 {
						return true, tf.EvalEarlyExitError{}
					}

					return true, nil
				},
				Then: tf.EvalNoop{},
			},

			// Normally we interpolate count as a preparation step before
			// a DynamicExpand, but an apply graph has pre-expanded nodes
			// and so the count would otherwise never be interpolated.
			//
			// This is redundant when there are multiple instances created
			// from the same config (count > 1) but harmless since the
			// underlying structures have mutexes to make this concurrency-safe.
			//
			// In most cases this isn't actually needed because we dealt with
			// all of the counts during the plan walk, but we do it here
			// for completeness because other code assumes that the
			// final count is always available during interpolation.
			//
			// Here we are just populating the interpolated value in-place
			// inside this RawConfig object, like we would in
			// NodeAbstractCountResource.
			/*&tf.EvalInterpolate{
				Config:        n.Config.RawCount,
				ContinueOnErr: true,
			},*/

			// We need to re-interpolate the config here, rather than
			// just using the diff's values directly, because we've
			// potentially learned more variable values during the
			// apply pass that weren't known when the diff was produced.
			/*&tf.EvalInterpolate{
				Config:   n.Config.RawConfig.Copy(),
				Resource: resource,
				Output:   &config,
			},*/

			&tf.EvalGetProvider{
				Name:   n.ResolvedProvider,
				Output: &provider,
			},

			// Make a new diff with our newly-interpolated config.
			/*&tf.EvalReadDataDiff{
				Info:     info,
				Config:   &config,
				Previous: &diff,
				Provider: &provider,
				Output:   &diff,
			},*/

			&tf.EvalReadDataApply{
				Info:     info,
				Diff:     &diff,
				Provider: &provider,
				Output:   &state,
			},

			&tf.EvalWriteState{
				Name:         stateId,
				ResourceType: n.Addr.Type,
				Provider:     n.ResolvedProvider,
				Dependencies: stateDeps,
				State:        &state,
			},

			// Clear the diff now that we've applied it, so
			// later nodes won't see a diff that's now a no-op.
			&tf.EvalWriteDiff{
				Name: stateId,
				Diff: nil,
			},

			&tf.EvalUpdateStateHook{},
		},
	}
}

func (n *nodePatchableResource) evalTreeManagedResource(
	stateId string, info *tf.InstanceInfo,
	resource *tf.Resource, stateDeps []string) tf.EvalNode {
	// Declare a bunch of variables that are used for state during
	// evaluation. Most of this are written to by-address below.
	var provider tf.ResourceProvider
	var diffApply *tf.InstanceDiff
	var state *tf.InstanceState
	//var resourceConfig *tf.ResourceConfig
	var err error
	var createNew bool
	var createBeforeDestroyEnabled bool

	return &tf.EvalSequence{
		Nodes: []tf.EvalNode{
			// Build the instance info
			&tf.EvalInstanceInfo{
				Info: info,
			},

			// Get the saved diff for apply
			&tf.EvalReadDiff{
				Name: stateId,
				Diff: &diffApply,
			},

			// We don't want to do any destroys
			&tf.EvalIf{
				If: func(ctx tf.EvalContext) (bool, error) {
					if diffApply == nil {
						return true, tf.EvalEarlyExitError{}
					}

					if diffApply.GetDestroy() && diffApply.GetAttributesLen() == 0 {
						return true, tf.EvalEarlyExitError{}
					}

					diffApply.SetDestroy(false)
					return true, nil
				},
				Then: tf.EvalNoop{},
			},

			/*&tf.EvalIf{
				If: func(ctx tf.EvalContext) (bool, error) {
					destroy := false
					if diffApply != nil {
						destroy = diffApply.GetDestroy() || diffApply.RequiresNew()
					}

					createBeforeDestroyEnabled =
						n.Config.Lifecycle.CreateBeforeDestroy &&
							destroy

					return createBeforeDestroyEnabled, nil
				},
				Then: &tf.EvalDeposeState{
					Name: stateId,
				},
			},*/

			// Normally we interpolate count as a preparation step before
			// a DynamicExpand, but an apply graph has pre-expanded nodes
			// and so the count would otherwise never be interpolated.
			//
			// This is redundant when there are multiple instances created
			// from the same config (count > 1) but harmless since the
			// underlying structures have mutexes to make this concurrency-safe.
			//
			// In most cases this isn't actually needed because we dealt with
			// all of the counts during the plan walk, but we need to do this
			// in order to support interpolation of resource counts from
			// apply-time-interpolated expressions, such as those in
			// "provisioner" blocks.
			//
			// Here we are just populating the interpolated value in-place
			// inside this RawConfig object, like we would in
			// NodeAbstractCountResource.
			/*&tf.EvalInterpolate{
				Config:        n.Config.RawCount,
				ContinueOnErr: true,
			},*/

			/*&tf.EvalInterpolate{
				Config:   n.Config.RawConfig.Copy(),
				Resource: resource,
				Output:   &resourceConfig,
			},*/
			/*&tf.EvalGetProvider{
				Name:   n.ResolvedProvider,
				Output: &provider,
			},*/
			/*&tf.EvalReadState{
				Name:   stateId,
				Output: &state,
			},*/
			// Re-run validation to catch any errors we missed, e.g. type
			// mismatches on computed values.
			/*&tf.EvalValidateResource{
				Provider:       &provider,
				Config:         &resourceConfig,
				ResourceName:   n.Config.Name,
				ResourceType:   n.Config.Type,
				ResourceMode:   n.Config.Mode,
				IgnoreWarnings: true,
			},*/
			/*&tf.EvalDiff{
				Info:       info,
				Config:     &resourceConfig,
				Resource:   n.Config,
				Provider:   &provider,
				Diff:       &diffApply,
				State:      &state,
				OutputDiff: &diffApply,
			},*/

			// Get the saved diff
			/*&tf.EvalReadDiff{
				Name: stateId,
				Diff: &diff,
			},*/

			// Compare the diffs
			/*&tf.EvalCompareDiff{
				Info: info,
				One:  &diff,
				Two:  &diffApply,
			},*/

			&tf.EvalGetProvider{
				Name:   n.ResolvedProvider,
				Output: &provider,
			},
			&tf.EvalReadState{
				Name:   stateId,
				Output: &state,
			},
			// Call pre-apply hook
			&tf.EvalApplyPre{
				Info:  info,
				State: &state,
				Diff:  &diffApply,
			},
			&tf.EvalApply{
				Info:      info,
				State:     &state,
				Diff:      &diffApply,
				Provider:  &provider,
				Output:    &state,
				Error:     &err,
				CreateNew: &createNew,
			},
			&tf.EvalWriteState{
				Name:         stateId,
				ResourceType: n.Addr.Type,
				Provider:     n.ResolvedProvider,
				Dependencies: stateDeps,
				State:        &state,
			},
			/*&tf.EvalApplyProvisioners{
				Info:           info,
				State:          &state,
				Resource:       n.Config,
				InterpResource: resource,
				CreateNew:      &createNew,
				Error:          &err,
				When:           config.ProvisionerWhenCreate,
			},*/
			&tf.EvalIf{
				If: func(ctx tf.EvalContext) (bool, error) {
					return createBeforeDestroyEnabled && err != nil, nil
				},
				Then: &tf.EvalUndeposeState{
					Name:  stateId,
					State: &state,
				},
				Else: &tf.EvalWriteState{
					Name:         stateId,
					ResourceType: n.Addr.Type,
					Provider:     n.ResolvedProvider,
					Dependencies: stateDeps,
					State:        &state,
				},
			},

			// We clear the diff out here so that future nodes
			// don't see a diff that is already complete. There
			// is no longer a diff!
			&tf.EvalWriteDiff{
				Name: stateId,
				Diff: nil,
			},

			&tf.EvalApplyPost{
				Info:  info,
				State: &state,
				Error: &err,
			},
			&tf.EvalUpdateStateHook{},
		},
	}
}
