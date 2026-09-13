package sandbox

import (
	"github.com/goairix/sandbox/internal/runtime"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ordinaryResourcesCompatible compares requested limits with the configuration
// actually used to prepare inventory, including runtime defaults.
func ordinaryResourcesCompatible(requested ResourceLimits, prepared PoolConfig) bool {
	tmpDisk := prepared.TmpDisk
	if tmpDisk == "" {
		tmpDisk = runtime.DefaultTmpDisk
	}
	return resourceQuantityCompatible(requested.Memory, prepared.Memory) &&
		resourceQuantityCompatible(requested.CPU, prepared.CPU) &&
		resourceQuantityCompatible(requested.Disk, prepared.Disk) &&
		resourceQuantityCompatible(requested.TmpDisk, tmpDisk)
}

func resourceQuantityCompatible(requested, prepared string) bool {
	if requested == "" {
		return true
	}
	request, err := resource.ParseQuantity(requested)
	if err != nil {
		return false
	}
	actual, err := resource.ParseQuantity(prepared)
	return err == nil && request.Cmp(actual) == 0
}

func fillOrdinaryResourceDefaults(spec *runtime.SandboxSpec, requested ResourceLimits, prepared PoolConfig) {
	if requested.Memory == "" {
		spec.Memory = prepared.Memory
		spec.MemoryRequest = prepared.MemoryRequest
	}
	if requested.CPU == "" {
		spec.CPU = prepared.CPU
		spec.CPURequest = prepared.CPURequest
	}
	if requested.Disk == "" {
		spec.Disk = prepared.Disk
	}
	if requested.TmpDisk == "" {
		spec.TmpDisk = prepared.TmpDisk
	}
}
