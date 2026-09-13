package main

import (
	"time"

	"github.com/goairix/sandbox/internal/config"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
)

// Cleanup and offline inspection retain scheduling identity but never wait for
// a newly requested node policy or enable new-policy authorization checks.
func kubernetesAppArmorOptions(cfg config.KubernetesConfig, profile string, inspection, drain bool) []k8sruntime.Option {
	options := []k8sruntime.Option{k8sruntime.WithNodeSelector(cfg.NodeSelector)}
	if !inspection && !drain && cfg.AppArmorLoaderName != "" {
		options = append(options, k8sruntime.WithAppArmorLoader(cfg.AppArmorLoaderName, cfg.AppArmorLoaderNamespace, profile, time.Duration(cfg.AppArmorLoaderTimeoutSeconds)*time.Second))
	}
	return options
}
