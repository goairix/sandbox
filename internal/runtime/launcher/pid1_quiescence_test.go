package launcher

import (
	"context"
	"testing"
)

func TestPID1QuiescenceRejectsManufacturedObservation(t *testing.T) {
	for _, o := range []*UserNamespaceQuiescenceObservation{nil, {}, {origin: &KernelBoundary{}}} {
		if err := o.RevalidateCurrent(context.Background()); err == nil {
			t.Fatal("manufactured observation accepted")
		}
	}
}
