package kubecontract

import (
	"fmt"
	"reflect"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	v1 "k8s.io/api/core/v1"
)

func validateEnrolledSecurity(pod *v1.Pod) error {
	c := pod.Spec.Containers[0]
	resources := c.Resources
	if len(resources.Claims) != 0 || len(resources.Limits) != 2 || len(resources.Requests) != 2 {
		return fmt.Errorf("bounded enrolled CPU and memory required")
	}
	for _, name := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
		limit, ok := resources.Limits[name]
		request, exists := resources.Requests[name]
		if !ok || !exists || limit.Sign() <= 0 || request.Sign() <= 0 || request.Cmp(limit) > 0 {
			return fmt.Errorf("invalid enrolled resource bound")
		}
	}
	expected := EnrolledPlainPodSpec(pod.Spec.NodeName, c.Image, c.Env[0].Value, resources, pod.Spec.Volumes)
	actual := pod.Spec.DeepCopy()
	if actual.PriorityClassName == "" && actual.Priority != nil && *actual.Priority == 0 {
		actual.Priority = nil
	}
	if actual.PriorityClassName == "" && actual.PreemptionPolicy != nil && *actual.PreemptionPolicy == v1.PreemptLowerPriority {
		actual.PreemptionPolicy = nil
	}
	// Fail closed for every unsupported Pod/container/security field, including
	// future API additions. Real admission defaults require explicit review.
	if !reflect.DeepEqual(*actual, expected) {
		return fmt.Errorf("enrolled Pod security or launch shape differs")
	}
	return nil
}

// EnrolledPlainPodSpec creates the fixed structural template, with copied inputs.
// Callers must validate dynamic inputs using InspectEnrolledPlainPod; this is
// neither an admission decision nor evidence of authority or node enforcement.
func EnrolledPlainPodSpec(nodeName, image, config string, resources v1.ResourceRequirements, volumes []v1.Volume) v1.PodSpec {
	no, yes, root, grace, seconds := false, true, int64(0), int64(30), int64(300)
	spec := v1.PodSpec{
		NodeName: nodeName, RestartPolicy: v1.RestartPolicyNever, SchedulerName: v1.DefaultSchedulerName,
		ServiceAccountName: "default", DeprecatedServiceAccount: "default", AutomountServiceAccountToken: &no, EnableServiceLinks: &no,
		ShareProcessNamespace: &no, TerminationGracePeriodSeconds: &grace, DNSPolicy: v1.DNSNone,
		DNSConfig:       &v1.PodDNSConfig{Nameservers: []string{"8.8.8.8", "1.1.1.1"}},
		SecurityContext: &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}},
		Volumes:         volumes,
		Tolerations:     []v1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute, TolerationSeconds: &seconds}, {Key: "node.kubernetes.io/unreachable", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute, TolerationSeconds: &seconds}},
		Containers: []v1.Container{{Name: "sandbox", Image: image, ImagePullPolicy: v1.PullIfNotPresent, Command: []string{EnrolledLauncherPath}, Args: []string{"serve-provisioned"}, WorkingDir: "/workspace", Resources: resources,
			TerminationMessagePath: v1.TerminationMessagePathDefault, TerminationMessagePolicy: v1.TerminationMessageReadFile,
			SecurityContext: &v1.SecurityContext{RunAsUser: &root, RunAsGroup: &root, RunAsNonRoot: &no, Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}}},
			Env:             []v1.EnvVar{{Name: launcherconfig.Environment, Value: config}},
			VolumeMounts:    []v1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}, {Name: "control", MountPath: "/run/sandbox-control"}, {Name: "clock", MountPath: "/run/sandbox-clock", ReadOnly: true}, {Name: "enrollment", MountPath: "/run/sandbox-control/enrollment.sock", ReadOnly: true}},
		}},
	}
	return *spec.DeepCopy()
}
