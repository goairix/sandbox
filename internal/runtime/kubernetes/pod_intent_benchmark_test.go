package kubernetes

import (
	"testing"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
)

func BenchmarkPodIntentDefault(b *testing.B) {
	desired, err := buildOrdinaryPod("runtime", sandboxruntime.SandboxSpec{ID: "dns-benchmark", Image: "example.test/sandbox:v1"})
	if err != nil {
		b.Fatal(err)
	}
	current := desired.DeepCopy()
	current.UID = "fixed-uid"
	current.Spec.Priority = new(int32)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !preparedPodIntentMatches(current, desired, false) {
			b.Fatal("unexpected mismatch")
		}
	}
}

func BenchmarkPodIntentConfiguredDNS(b *testing.B) {
	r := &Runtime{}
	WithDNSAdmission(true, configuredDNSOptions())(r)
	desired, err := buildOrdinaryPod("runtime", sandboxruntime.SandboxSpec{ID: "dns-benchmark", Image: "example.test/sandbox:v1"})
	if err != nil {
		b.Fatal(err)
	}
	if err := r.applyDNSAdmission(desired, nil); err != nil {
		b.Fatal(err)
	}
	current := desired.DeepCopy()
	current.UID = "fixed-uid"
	current.Spec.Priority = new(int32)
	current.Spec.DNSConfig.Options[0], current.Spec.DNSConfig.Options[1] = current.Spec.DNSConfig.Options[1], current.Spec.DNSConfig.Options[0]
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !r.podIntentMatches(current, desired, false) {
			b.Fatal("unexpected mismatch")
		}
	}
}
