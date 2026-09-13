package kubernetes

import "fmt"

// WithNetworkPolicyProvider allows an operator to override API-based discovery
// when obsolete Cilium CRDs remain in a standard/Calico/VPC cluster.
func WithNetworkPolicyProvider(provider string) Option {
	return func(r *Runtime) { r.networkPolicyProvider = provider }
}

func (r *Runtime) configureNetworkPolicyProvider() error {
	r.hasCiliumAPI = r.hasCiliumAPI || r.hasCilium
	switch r.networkPolicyProvider {
	case "", "auto":
		// Compatibility discovery. Operators must verify the active provider.
		r.hasCilium = r.hasCiliumAPI
	case "standard":
		r.hasCilium = false
	case "cilium":
		if !r.hasCiliumAPI {
			return fmt.Errorf("cilium network policy provider selected but API is unavailable")
		}
		r.hasCilium = true
	default:
		return fmt.Errorf("network_policy_provider must be auto, standard or cilium")
	}
	return nil
}

func (r *Runtime) ciliumAPIAvailable() bool { return r.hasCiliumAPI || r.hasCilium }
