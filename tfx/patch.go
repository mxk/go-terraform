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

// patch performs a direct apply operation that does not require a config.
func patch(opts *tf.ContextOpts) (*tf.State, error) {
	orig, state := opts.State, opts.State.DeepCopy()
	opts.State = state
	c, err := tf.NewContext(opts)
	if opts.State = orig; err != nil {
		return nil, err
	}
	components := (&tf.ContextGraphWalker{Context: c}).
		EnterPath(tf.RootModulePath).(*tf.BuiltinEvalContext).Components

	// Build graph
	graph, err := (&PatchGraphBuilder{
		Diff:      opts.Diff,
		State:     opts.State,
		Providers: components.ResourceProviders(),
		Targets:   opts.Targets,
		Destroy:   opts.Destroy,
		Validate:  true,
	}).Build(tf.RootModulePath)
	if err != nil {
		return nil, err
	}

	// Walk graph
	walkApplyOnce.Do(func() {
		// Hack to get walkApply, local context avoids state deep copy
		var c tf.Context
		walkApply.Operation = c.Interpolater().Operation
	})
	walker := &tf.ContextGraphWalker{
		Context:     c,
		Operation:   walkApply.Operation,
		StopContext: context.Background(),
	}
	err = graph.Walk(walker)
	if len(walker.ValidationErrors) > 0 {
		err = multierror.Append(err, walker.ValidationErrors...)
	}

	// TODO: Stop providers and provisioners
	return state, err
}

type PatchGraphBuilder struct {
	Diff          *tf.Diff
	State         *tf.State
	Providers     []string
	Targets       []string
	DisableReduce bool
	Destroy       bool
	Validate      bool
}

func (b *PatchGraphBuilder) Build(path []string) (*tf.Graph, error) {
	return (&tf.BasicGraphBuilder{
		Steps:    b.Steps(),
		Validate: b.Validate,
		Name:     "PatchGraphBuilder",
	}).Build(path)
}

func (b *PatchGraphBuilder) Steps() []tf.GraphTransformer {
	concreteProvider := func(a *tf.NodeAbstractProvider) dag.Vertex {
		return &tf.NodeApplyableProvider{
			NodeAbstractProvider: a,
		}
	}
	concreteResource := func(a *tf.NodeAbstractResource) dag.Vertex {
		return &NodePatchableResource{
			NodeApplyableResource: tf.NodeApplyableResource{
				NodeAbstractResource: a,
			},
		}
	}
	steps := []tf.GraphTransformer{
		// Creates all the nodes represented in the diff.
		&tf.DiffTransformer{
			Concrete: concreteResource,
			Diff:     b.Diff,
			State:    b.State,
		},

		// Attach the state
		&tf.AttachStateTransformer{State: b.State},

		// Add providers
		tf.TransformProviders(b.Providers, concreteProvider, nil),

		// Destruction ordering
		&tf.DestroyEdgeTransformer{State: b.State},
		tf.GraphTransformIf(
			func() bool { return !b.Destroy },
			&tf.CBDEdgeTransformer{State: b.State},
		),

		// TODO: Should provisioners be here at all?

		// Provisioner-related transformations
		&tf.ProvisionerTransformer{},

		// Connect references so ordering is correct
		&tf.ReferenceTransformer{},

		// Handle destroy time transformations for output and local values.
		// Reverse the edges from outputs and locals, so that
		// interpolations don't fail during destroy.
		// Create a destroy node for outputs to remove them from the state.
		// Prune unreferenced values, which may have interpolations that can't
		// be resolved.
		tf.GraphTransformIf(
			func() bool { return b.Destroy },
			tf.GraphTransformMulti(
				&tf.DestroyValueReferenceTransformer{},
				&tf.DestroyOutputTransformer{},
				&tf.PruneUnusedValuesTransformer{},
			),
		),

		// Add the node to fix the state count boundaries
		&tf.CountBoundaryTransformer{},

		// Target
		&tf.TargetsTransformer{Targets: b.Targets},

		// Close opened plugin connections
		&tf.CloseProviderTransformer{},
		&tf.CloseProvisionerTransformer{},

		// Single root
		&tf.RootTransformer{},
	}
	if !b.DisableReduce {
		// Perform the transitive reduction to make our graph a bit
		// more sane if possible (it usually is possible).
		steps = append(steps, &tf.TransitiveReductionTransformer{})
	}
	return steps
}

type NodePatchableResource struct {
	tf.NodeApplyableResource
	Config struct{}
}

func (n *NodePatchableResource) EvalTree() tf.EvalNode {
	addr := n.NodeAbstractResource.Addr

	// stateId is the ID to put into the state
	k := tf.ResourceStateKey{
		Name:  addr.Name,
		Type:  addr.Type,
		Mode:  addr.Mode,
		Index: addr.Index,
	}
	stateId := k.String()

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
		return n.evalTreeManagedResource(
			stateId, info, resource, stateDeps,
		)
	case config.DataResourceMode:
		return n.evalTreeDataResource(
			stateId, info, resource, stateDeps)
	default:
		panic(fmt.Errorf("unsupported resource mode %s", addr.Mode))
	}
}

func (n *NodePatchableResource) evalTreeDataResource(
	stateId string, info *tf.InstanceInfo,
	resource *tf.Resource, stateDeps []string) tf.EvalNode {
	var provider tf.ResourceProvider
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

			&tf.EvalGetProvider{
				Name:   n.ResolvedProvider,
				Output: &provider,
			},

			&tf.EvalReadDataApply{
				Info:     info,
				Diff:     &diff,
				Provider: &provider,
				Output:   &state,
			},

			&tf.EvalWriteState{
				Name:         stateId,
				ResourceType: n.ResourceState.Type,
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

func (n *NodePatchableResource) evalTreeManagedResource(
	stateId string, info *tf.InstanceInfo,
	resource *tf.Resource, stateDeps []string) tf.EvalNode {
	// Declare a bunch of variables that are used for state during
	// evaluation. Most of this are written to by-address below.
	var provider tf.ResourceProvider
	var diff *tf.InstanceDiff
	var state *tf.InstanceState
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
				Diff: &diff,
			},

			// We don't want to do any destroys
			&tf.EvalIf{
				If: func(ctx tf.EvalContext) (bool, error) {
					if diff == nil {
						return true, tf.EvalEarlyExitError{}
					}

					if diff.GetDestroy() && diff.GetAttributesLen() == 0 {
						return true, tf.EvalEarlyExitError{}
					}

					diff.SetDestroy(false)
					return true, nil
				},
				Then: tf.EvalNoop{},
			},

			// TODO: Is lifecycle info saved in the state?
			&tf.EvalIf{
				If: func(ctx tf.EvalContext) (bool, error) {
					return false, nil
				},
				Then: &tf.EvalDeposeState{
					Name: stateId,
				},
			},

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
				Diff:  &diff,
			},
			&tf.EvalApply{
				Info:      info,
				State:     &state,
				Diff:      &diff,
				Provider:  &provider,
				Output:    &state,
				Error:     &err,
				CreateNew: &createNew,
			},
			&tf.EvalWriteState{
				Name:         stateId,
				ResourceType: n.ResourceState.Type,
				Provider:     n.ResolvedProvider,
				Dependencies: stateDeps,
				State:        &state,
			},
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
					ResourceType: n.ResourceState.Type,
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
