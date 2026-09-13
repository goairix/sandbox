package kubernetes

import (
	"context"
	"fmt"
	"net/netip"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var calicoPoolGVR = schema.GroupVersionResource{Group: "crd.projectcalico.org", Version: "v1", Resource: "ippools"}

// Calico IPAM can borrow blocks across Nodes. Include disabled pools: disabling
// new assignments does not prove that their existing allocations disappeared.
func calicoAllocationFamilies(ctx context.Context, client dynamic.Interface, add func(string) error) (map[int]bool, error) {
	families := map[int]bool{}
	continuation := ""
	count := 0
	for page := 0; page < networkInventoryMaxPages; page++ {
		list, err := client.Resource(calicoPoolGVR).List(ctx, metav1.ListOptions{Limit: networkInventoryPageSize, Continue: continuation})
		if apierrors.IsNotFound(err) && page == 0 {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("inventory Calico IPPool allocation ranges; configure complete pod_cidrs and service_cidrs: %w", err)
		}
		count += len(list.Items)
		if count > networkInventoryMaxItems {
			return nil, fmt.Errorf("calico IPPool inventory limit exceeded")
		}
		for _, pool := range list.Items {
			cidr, found, err := unstructured.NestedString(pool.Object, "spec", "cidr")
			if err != nil || !found || cidr == "" {
				return nil, fmt.Errorf("calico IPPool allocation CIDR is missing or invalid")
			}
			if err := add(cidr); err != nil {
				return nil, err
			}
			prefix, _ := netip.ParsePrefix(cidr)
			families[prefix.Addr().BitLen()] = true
		}
		continuation = list.GetContinue()
		if continuation == "" {
			if count == 0 {
				return nil, fmt.Errorf("calico IPPool allocation inventory is empty; configure complete pod_cidrs and service_cidrs")
			}
			return families, nil
		}
	}
	return nil, fmt.Errorf("calico IPPool inventory page limit exceeded")
}
