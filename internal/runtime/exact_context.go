package runtime

import "context"

type exactRuntimeContextKey struct{}

// WithExactRuntimeRef binds trusted workspace copy operations to an immutable
// instance. Kubernetes additionally verifies this identity inside each exec,
// since the Kubernetes exec subresource has no UID precondition.
func WithExactRuntimeRef(ctx context.Context, ref RuntimeRef) context.Context {
	return context.WithValue(ctx, exactRuntimeContextKey{}, ref)
}

func ExactRuntimeRefFromContext(ctx context.Context) (RuntimeRef, bool) {
	ref, ok := ctx.Value(exactRuntimeContextKey{}).(RuntimeRef)
	return ref, ok
}
