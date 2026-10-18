package deployment

import (
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

// cleanupOpenManifest adapts strict, explicitly selected mounted versions to
// the common opener. Inventory is only an expected profile; storage remains
// responsible for checking the persisted readiness and digest before use.
type cleanupOpenManifest struct {
	cleanupManifest
	Inventory *state.PreparationInventoryOptions
	Detached  *state.DetachedCleanupOptions
}

func parseCleanupOpenManifest(wire []byte, version int) (cleanupOpenManifest, error) {
	switch version {
	case 1, 2:
		m, err := parseCleanupManifestVersion(wire, version)
		if err != nil {
			return cleanupOpenManifest{}, err
		}
		return cleanupOpenManifest{cleanupManifest: m}, nil
	case 3:
		m, err := parseColdQuotaCleanupManifest(wire)
		if err != nil {
			return cleanupOpenManifest{}, err
		}
		inventory := m.Inventory
		return cleanupOpenManifest{cleanupManifest: m.Cleanup, Inventory: &inventory, Detached: m.Detached}, nil
	default:
		return cleanupOpenManifest{}, errConfiguration
	}
}

// Copy the expected profile into the backend options so separate opens cannot
// share mutable inventory configuration. Only the explicit v3 parser supplies
// this profile; the original mounted entries never opt into cold quota return.
func (m cleanupOpenManifest) backendOptions(namespace state.Namespace, config launcherconfig.Config, clock p.AuthorityClock, worker *launcher.KernelBoundary, d *LocalCleanupDeployment) state.Options {
	options := m.cleanupManifest.backendOptions(namespace, config, clock, worker, d)
	if m.Version == 3 && m.Inventory != nil {
		inventory := *m.Inventory
		options.PreparationInventory = &inventory
		options.LocalCleanup.ColdRuntimeQuotaReturn = true
		if m.Detached != nil {
			// The common opener passes this owned configuration to Backend.New
			// before starting the original automatic consumer. HTTP admission
			// separately authenticates each actual target against this pin.
			detached := *m.Detached
			options.DetachedCleanup = &detached
		}
	}
	return options
}

func (m cleanupOpenManifest) workerOptions() state.DueWorkerOptions {
	return state.DueWorkerOptions{
		WorkerID: m.Worker.WorkerID, Partitions: m.Worker.Partitions,
		RangeSlots: m.Worker.RangeSlots, ClaimSlots: m.Worker.ClaimSlots, RelistSlot: m.Worker.RelistSlot,
		StageTTL: cleanupDuration(m.Worker.StageTTL),
		// Each automatic entry selects its strict version before common setup;
		// both parsers require an explicit true automatic_cleanup field.
		AutomaticCleanup: m.Version == 2 || m.Version == 3,
	}
}
