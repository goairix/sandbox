package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"unicode/utf8"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (n Namespace) execEffectKey(r OperationReference) (string, error) {
	if r.Validate() != nil || r.Kind != OperationData {
		return "", ErrInvalidRecord
	}
	if r.Namespace != n.Root() {
		return "", ErrIdentityMismatch
	}
	return n.Key("p", fmt.Sprintf("%02x", r.Partition), "intents", r.OperationID, "exec-start")
}

func encodeExecEffectRecord(r ExecEffectRecord) (string, error) {
	r.Ticket = bytes.Clone(r.Ticket)
	if err := r.Validate(); err != nil {
		return "", err
	}
	var value bytes.Buffer
	encoder := json.NewEncoder(&value)
	// TicketDigest identifies compact opaque wire, including its HTML bytes.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(r); err != nil {
		return "", ErrInvalidRecord
	}
	wire := bytes.TrimSuffix(value.Bytes(), []byte("\n"))
	if len(wire) > 16384 || !utf8.Valid(wire) {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func decodeExecEffectRecord(kv *mvccpb.KeyValue, dst *ExecEffectRecord) error {
	if dst == nil || !immutableDispatchKV(kv) || len(kv.Value) > 16384 || !utf8.Valid(kv.Value) {
		return ErrCorruptRecord
	}
	wire := bytes.Clone(kv.Value)
	if strictPreparationMetadata(wire, reflect.TypeOf(ExecEffectRecord{})) != nil {
		return ErrCorruptRecord
	}
	var fresh ExecEffectRecord
	if json.Unmarshal(wire, &fresh) != nil || fresh.Validate() != nil {
		return ErrCorruptRecord
	}
	expected := fmt.Sprintf("%sp/%02x/intents/%s/exec-start", fresh.Operation.Reference.Namespace, fresh.Operation.Reference.Partition, fresh.Operation.Reference.OperationID)
	if string(kv.Key) != expected {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}
