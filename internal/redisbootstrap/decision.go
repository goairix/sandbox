package redisbootstrap

import (
	"errors"
	"fmt"
)

// Role records a previously persisted role, not permission to promote.
type Role string

const (
	// Primary is a persisted prior primary role.
	Primary Role = "primary"
	// Replica is a persisted replica role.
	Replica Role = "replica"
)

// PersistedState is verified local Redis role and Sentinel configuration evidence.
type PersistedState struct {
	Member        Member
	Role          Role
	PrimaryDNS    string
	SentinelEpoch uint64
}

// VolumeState is a caller-verified fresh observation of one fixed member's PVC.
// Empty must include configuration, identity and Redis data; absence is not emptiness.
type VolumeState struct {
	Empty     bool
	Identity  *VolumeIdentity
	Persisted *PersistedState
}

// SentinelEvidence is a reachable fixed member's authenticated persisted observation.
// Persisted means the caller validated its retained PVC identity and Sentinel state.
// A matching report is discovery evidence, not election authority.
type SentinelEvidence struct {
	Member     Member
	ClusterID  string
	Persisted  bool
	PrimaryDNS string
	Epoch      uint64
}

// SelectedEvidence is unanimous evidence at the highest reachable epoch with a majority.
type SelectedEvidence struct {
	PrimaryDNS string
	Epoch      uint64
}

// SelectEvidence rejects duplicate voters, untrusted identities and higher minority epochs.
// It never authorizes a role transition or REPLICAOF NO ONE.
func SelectEvidence(c ClusterState, evidence []SentinelEvidence) (SelectedEvidence, error) {
	if err := c.Validate(); err != nil {
		return SelectedEvidence{}, err
	}
	if len(evidence) < 2 || len(evidence) > 3 {
		return SelectedEvidence{}, errors.New("need two or three retained member observations")
	}
	seen := map[int]bool{}
	var max uint64
	for _, e := range evidence {
		if err := e.Member.Validate(c); err != nil {
			return SelectedEvidence{}, err
		}
		if e.ClusterID != c.ClusterID || !e.Persisted || seen[e.Member.Ordinal] || !containsDNS(c, e.PrimaryDNS) {
			return SelectedEvidence{}, errors.New("invalid or duplicate persisted evidence")
		}
		seen[e.Member.Ordinal] = true
		if e.Epoch > max {
			max = e.Epoch
		}
	}
	selected := SelectedEvidence{Epoch: max}
	count := 0
	for _, e := range evidence {
		if e.Epoch == max {
			if count > 0 && selected.PrimaryDNS != e.PrimaryDNS {
				return SelectedEvidence{}, errors.New("conflicting primary at highest epoch")
			}
			selected.PrimaryDNS = e.PrimaryDNS
			count++
		}
	}
	if count < 2 {
		return SelectedEvidence{}, errors.New("highest reachable epoch lacks retained majority")
	}
	return selected, nil
}

func containsDNS(c ClusterState, dns string) bool {
	for _, member := range c.Members {
		if member == dns {
			return true
		}
	}
	return false
}

// Action is a bounded startup instruction; none grants normal promotion authority.
type Action string

const (
	// Wait blocks startup until external evidence or operator recovery is available.
	Wait Action = "wait"
	// SeedPrimary applies only to a genuinely empty new Pending inventory, ordinal zero.
	SeedPrimary Action = "seed-primary"
	// SeedReplica applies only to a genuinely empty new Pending inventory.
	SeedReplica Action = "seed-replica"
	// ResumePersisted preserves a configured Pending replica without role changes.
	ResumePersisted Action = "resume-persisted"
	// FollowPrimary configures a replica to follow the known current primary.
	FollowPrimary Action = "follow-primary"
	// RestorePriorPrimary restarts only a matching already-persisted primary role.
	RestorePriorPrimary Action = "restore-prior-primary"
)

// Decision carries a startup instruction and selected primary discovery evidence.
type Decision struct {
	Action     Action
	PrimaryDNS string
	Epoch      uint64
}

// DecideBootstrap consumes an authenticated fresh complete inventory supplied by a caller.
// It cannot prove the inventory's authenticity, coordinate concurrent fresh launch,
// or execute elections. Marker-only partial initialization intentionally waits.
func DecideBootstrap(c *ClusterState, local Member, inventory [3]VolumeState, evidence []SentinelEvidence) (Decision, error) {
	wait := Decision{Action: Wait}
	if c == nil {
		return wait, errors.New("namespace cluster state is required; retained PVCs must never be reseeded")
	}
	if err := local.Validate(*c); err != nil {
		return wait, err
	}
	fresh := true
	primaryAtEpoch := make(map[uint64]string)
	for i, v := range inventory {
		if v.Empty {
			if v.Identity != nil || v.Persisted != nil {
				return wait, errors.New("empty volume contradicts retained state")
			}
			continue
		}
		fresh = false
		if v.Identity == nil {
			return wait, fmt.Errorf("member %d nonempty PVC has no identity", i)
		}
		if err := v.Identity.Validate(*c); err != nil {
			return wait, err
		}
		if v.Identity.Member.Ordinal != i {
			return wait, errors.New("inventory identity in wrong ordinal slot")
		}
		if v.Persisted != nil {
			p := v.Persisted
			if p.Member != v.Identity.Member || v.Identity.InitialConfig != Configured || !containsDNS(*c, p.PrimaryDNS) || (p.Role != Primary && p.Role != Replica) || (p.Role == Primary && p.PrimaryDNS != p.Member.DNS) {
				return wait, errors.New("persisted configuration does not match local identity or role")
			}
			if primary, seen := primaryAtEpoch[p.SentinelEpoch]; seen && primary != p.PrimaryDNS {
				return wait, errors.New("retained Sentinel configurations disagree on primary at the same epoch")
			}
			primaryAtEpoch[p.SentinelEpoch] = p.PrimaryDNS
		}
	}
	if fresh && c.Phase == Pending && len(evidence) == 0 {
		action := SeedReplica
		if local.Ordinal == 0 {
			action = SeedPrimary
		}
		return Decision{Action: action, PrimaryDNS: c.Members[0]}, nil
	}
	v := inventory[local.Ordinal]
	if !v.Empty && (v.Identity.InitialConfig != Configured || v.Persisted == nil) {
		return wait, nil
	}
	selected, err := SelectEvidence(*c, evidence)
	if err != nil {
		if len(evidence) == 0 && c.Phase == Pending && !v.Empty && v.Persisted.Role == Replica {
			return Decision{Action: ResumePersisted, PrimaryDNS: v.Persisted.PrimaryDNS, Epoch: v.Persisted.SentinelEpoch}, nil
		}
		return wait, nil
	}
	for _, observation := range evidence {
		retained := inventory[observation.Member.Ordinal]
		if retained.Empty || retained.Identity == nil || retained.Identity.InitialConfig != Configured || retained.Persisted == nil {
			return wait, errors.New("discovery evidence came from missing or incomplete retained state")
		}
		if primary, seen := primaryAtEpoch[observation.Epoch]; seen && primary != observation.PrimaryDNS {
			return wait, errors.New("live discovery contradicts retained or discovered Sentinel primary at the same epoch")
		}
		primaryAtEpoch[observation.Epoch] = observation.PrimaryDNS
		if retained.Persisted.SentinelEpoch == observation.Epoch && retained.Persisted.PrimaryDNS != observation.PrimaryDNS {
			return wait, errors.New("live discovery contradicts its retained Sentinel primary at the same epoch")
		}
	}
	for _, retained := range inventory {
		if retained.Persisted != nil && retained.Persisted.SentinelEpoch > selected.Epoch {
			return wait, errors.New("reachable retained inventory has a higher Sentinel epoch")
		}
		if retained.Persisted != nil && retained.Persisted.SentinelEpoch == selected.Epoch && retained.Persisted.PrimaryDNS != selected.PrimaryDNS {
			return wait, errors.New("retained Sentinel primary contradicts selected discovery at the same epoch")
		}
	}
	if selected.PrimaryDNS == local.DNS {
		if v.Empty || v.Persisted == nil || v.Persisted.Role != Primary || v.Persisted.Member != local || v.Persisted.PrimaryDNS != selected.PrimaryDNS || v.Persisted.SentinelEpoch != selected.Epoch {
			return wait, errors.New("discovery cannot promote a replica or restore unmatched prior primary")
		}
		return Decision{Action: RestorePriorPrimary, PrimaryDNS: selected.PrimaryDNS, Epoch: selected.Epoch}, nil
	}
	return Decision{Action: FollowPrimary, PrimaryDNS: selected.PrimaryDNS, Epoch: selected.Epoch}, nil
}
