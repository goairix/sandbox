package etcd

// SandboxReadState contains metadata only; it grants no runtime operation,
// health, or expiration authority.
type SandboxReadState struct {
	Owner     *WorkspaceOwnerRecord
	Fence     *WorkspaceFenceRecord
	Placement *SandboxPlacementRecord
	Control   *SandboxControlRecord
}

// SandboxReadView is evidence from one coherent metadata snapshot. Its private
// self and origin pins prevent a copied view from becoming confirmation input.
type SandboxReadView struct {
	self          *SandboxReadView
	origin        *Backend
	keys          [4]string
	mods          [4]int64
	revision      int64
	records       SandboxReadState
	identityValue string
	restoreEpoch  string
	clusterID     uint64
}

// Records returns fresh ownership of every record and runtime pointer.
func (v *SandboxReadView) Records() SandboxReadState {
	if v == nil || v.self != v {
		return SandboxReadState{}
	}
	r := v.records
	owner, fence, placement, control := *r.Owner, *r.Fence, *r.Placement, *r.Control
	if owner.Runtime != nil {
		runtime := *owner.Runtime
		owner.Runtime = &runtime
	}
	if control.Runtime != nil {
		runtime := *control.Runtime
		control.Runtime = &runtime
	}
	return SandboxReadState{Owner: &owner, Fence: &fence, Placement: &placement, Control: &control}
}

func (v *SandboxReadView) Revision() int64 {
	if v == nil || v.self != v {
		return 0
	}
	return v.revision
}

func (v *SandboxReadView) ControlRevision() int64 {
	if v == nil || v.self != v {
		return 0
	}
	return v.mods[3]
}
