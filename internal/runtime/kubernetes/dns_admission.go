package kubernetes

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/goairix/sandbox/internal/kubecontract"
	corev1 "k8s.io/api/core/v1"
)

// WithDNSAdmission pins administrator-declared DNS options and the optional
// NodeLocal opt-out. The closure, each Runtime, and each Pod own separate copies.
// Invalid options are rejected before startup verification or resource writes.
func WithDNSAdmission(disable bool, options []kubecontract.DNSOption) Option {
	owned, err := kubecontract.ValidateDNSOptions(options)
	var contract string
	if err == nil && (disable || len(owned) != 0) {
		canonical := owned
		if canonical == nil {
			canonical = []kubecontract.DNSOption{}
		}
		encoded, _ := json.Marshal(struct {
			Disable bool                     `json:"disableNodeLocalDNSInjection"`
			Options []kubecontract.DNSOption `json:"dnsOptions"`
		}{disable, canonical}) // bool/string-only fields cannot fail to marshal.
		contract = fmt.Sprintf("%x", sha256.Sum256(encoded))
	}
	return func(r *Runtime) {
		r.disableNodeLocalDNSInjection = disable
		r.dnsOptions = slices.Clone(owned)
		r.dnsAdmissionError = err
		r.dnsAdmissionContract = contract
	}
}

func (r *Runtime) validateDNSAdmissionLabels(labels map[string]string) error {
	if r.dnsAdmissionError != nil {
		return r.dnsAdmissionError
	}
	if r.disableNodeLocalDNSInjection {
		if value, present := labels[kubecontract.NodeLocalDNSInjectionLabel]; present && value != "disabled" {
			return errors.New("NodeLocal DNS opt-out label is controlled by the Kubernetes runtime")
		}
	}
	return nil
}

func dnsPodOptions(options []kubecontract.DNSOption) []corev1.PodDNSConfigOption {
	result := make([]corev1.PodDNSConfigOption, len(options))
	for i, option := range options {
		value := option.Value
		result[i] = corev1.PodDNSConfigOption{Name: option.Name, Value: &value}
	}
	return result
}

func (r *Runtime) applyDNSAdmission(pod *corev1.Pod, labels map[string]string) error {
	if err := r.validateDNSAdmissionLabels(labels); err != nil {
		return err
	}
	if r.disableNodeLocalDNSInjection {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[kubecontract.NodeLocalDNSInjectionLabel] = "disabled"
	}
	if len(r.dnsOptions) != 0 {
		if pod.Spec.DNSConfig == nil {
			pod.Spec.DNSConfig = &corev1.PodDNSConfig{}
		}
		pod.Spec.DNSConfig.Options = dnsPodOptions(r.dnsOptions)
	}
	return nil
}

// dnsOptionsMatch accepts only the full configured set. Ordering and the nil
// representation of an explicitly configured valueless switch are immaterial.
func dnsOptionsMatch(actual []corev1.PodDNSConfigOption, expected []kubecontract.DNSOption) bool {
	if len(actual) != len(expected) {
		return false
	}
	var seen uint8
	for _, option := range actual {
		found := false
		for i, pinned := range expected {
			if option.Name != pinned.Name {
				continue
			}
			bit := uint8(1 << i)
			if seen&bit != 0 {
				return false
			}
			if option.Value == nil {
				if pinned.Name != "single-request-reopen" || pinned.Value != "" {
					return false
				}
			} else if *option.Value != pinned.Value {
				return false
			}
			seen |= bit
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func normalizePinnedDNSOptions(current, desired *corev1.Pod, options []kubecontract.DNSOption) bool {
	if len(options) == 0 {
		return true
	} // Default retains the existing exact comparison.
	if current.Spec.DNSConfig == nil || desired.Spec.DNSConfig == nil ||
		!dnsOptionsMatch(current.Spec.DNSConfig.Options, options) || !dnsOptionsMatch(desired.Spec.DNSConfig.Options, options) {
		return false
	}
	current.Spec.DNSConfig.Options = dnsPodOptions(options)
	desired.Spec.DNSConfig.Options = dnsPodOptions(options)
	return true
}

func (r *Runtime) podIntentMatches(current, desired *corev1.Pod, allowScheduledNodeName bool) bool {
	if r.dnsAdmissionError != nil {
		return false
	}
	if r.disableNodeLocalDNSInjection && desired != nil && desired.Labels[kubecontract.NodeLocalDNSInjectionLabel] != "disabled" {
		return false
	}
	return preparedPodIntentMatchesWithDNS(current, desired, allowScheduledNodeName, r.dnsOptions)
}

func (r *Runtime) podIntentMismatchReason(current, desired *corev1.Pod, allowScheduledNodeName bool) string {
	if r.dnsAdmissionError != nil {
		return "spec.DNSConfig"
	}
	if r.disableNodeLocalDNSInjection && desired != nil && desired.Labels[kubecontract.NodeLocalDNSInjectionLabel] != "disabled" {
		return "labels"
	}
	return preparedPodIntentMismatchReasonWithDNS(current, desired, allowScheduledNodeName, r.dnsOptions)
}
