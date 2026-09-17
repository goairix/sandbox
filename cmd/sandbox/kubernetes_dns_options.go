package main

import (
	"github.com/goairix/sandbox/internal/config"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
)

// DNS intent also identifies pools during inspection/drain; unlike loader
// startup verification, it must not be omitted for cleanup commands.
func kubernetesDNSOptions(cfg config.KubernetesConfig) []k8sruntime.Option {
	return []k8sruntime.Option{k8sruntime.WithDNSAdmission(cfg.DisableNodeLocalDNSInjection, cfg.DNSOptions)}
}
