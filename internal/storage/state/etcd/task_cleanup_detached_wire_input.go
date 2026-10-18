package etcd

import (
	"reflect"
	"time"
)

// These checks precede all schema encoding and native comparison construction.
// The broad scalar DTO bound only prevents unbounded inputs reaching validators;
// original typed validators and encoders still impose their narrower semantics
// and complete encoded-record caps. Opaque JSON is counted, never expanded.
func cleanupDetachedWireInputBounds(in cleanupDetachedWireInput) error {
	if in.bundle == nil || in.bundle.checkpoint == nil || len(in.bundle.fences) != 8 || in.publication == nil || in.snapshot.LocalCleanup != nil {
		return ErrInvalidRecord
	}
	for i, fence := range in.bundle.fences {
		if len(fence.key) == 0 || len(fence.key) > maxNamespaceKeyBytes || len(fence.value) > cleanupExecutionBytes {
			return ErrInvalidRecord
		}
		for _, previous := range in.bundle.fences[:i] {
			if previous.key == fence.key {
				return ErrInvalidRecord
			}
		}
	}
	checkpoint := in.bundle.checkpoint
	if len(checkpoint.Key) == 0 || len(checkpoint.Key) > maxNamespaceKeyBytes || len(checkpoint.Value) > cleanupExecutionBytes || len(in.issuer.Record.Certificate) > 4096 || len(in.snapshot.Payload) > 65536 || len(in.publication.Proof) > 8192 {
		return ErrInvalidRecord
	}
	e := in.publication.Enrollment
	for _, wire := range [][]byte{e.Request.RuntimeCertificate, e.Request.Activation, e.Request.IssuerCertificate, e.ObserverCertificate, e.Receipt} {
		// Each is contained in an original native companion capped at8192;
		// encoded companion and signature checks remain in the source reader.
		if len(wire) > 8192 {
			return ErrInvalidRecord
		}
	}
	for _, record := range []any{in.bundle.task, in.bundle.initial, in.snapshot, in.intent, in.publication.Record, in.publication.Reference, in.issuer.Record, in.workspace} {
		if !cleanupDetachedWireScalarBounds(reflect.ValueOf(record), 0) {
			return ErrInvalidRecord
		}
	}
	return nil
}

func cleanupDetachedWireScalarBounds(value reflect.Value, depth int) bool {
	if depth > 8 {
		return false
	}
	switch value.Kind() {
	case reflect.Struct:
		if value.NumField() > 64 {
			return false
		}
		for i := 0; i < value.NumField(); i++ {
			if !cleanupDetachedWireScalarBounds(value.Field(i), depth+1) {
				return false
			}
		}
	case reflect.String:
		return value.Len() <= maxNamespaceKeyBytes
	case reflect.Slice:
		return value.Type().Elem().Kind() == reflect.Uint8 && value.Len() <= 65536
	case reflect.Pointer:
		// Only time.Time's shared Location may appear behind these fixed DTOs.
		// It is never encoded or copied as a native source value.
		return value.IsNil() || value.Type() == reflect.TypeOf((*time.Location)(nil))
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
	return true
}
