package deployment

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

// coldQuotaCleanupManifest is the expected configuration for the explicit v3
// automatic quota worker. Parsing grants no inventory readiness: its
// consumer must retain the existing storage ready-record, profile-digest and
// restore-epoch checks. The mounted v1/v2 constructors deliberately reject v3.
type coldQuotaCleanupManifest struct {
	Cleanup   cleanupManifest
	Inventory state.PreparationInventoryOptions
}

// A slice preserves the wire length until every count has been checked. Direct
// decoding into the storage array would accept missing counts as implicit zero.
// This cold-only profile accepts no quota partition, ready flag or digest.
type coldQuotaCleanupProfile struct {
	QuotaSlots []uint32 `json:"quota_slots"`
}

// The embedded legacy schema keeps the common mounted fields identical without
// allowing preparation into the original v1/v2 parser or its constructors.
type coldQuotaCleanupManifestWire struct {
	cleanupManifest
	Preparation coldQuotaCleanupProfile `json:"preparation"`
}

func parseColdQuotaCleanupManifest(wire []byte) (coldQuotaCleanupManifest, error) {
	var result coldQuotaCleanupManifest
	if len(wire) == 0 || len(wire) > 65536 || !utf8.Valid(wire) {
		return result, errConfiguration
	}

	d := json.NewDecoder(bytes.NewReader(wire))
	if strictValue(d, 0) != nil {
		return result, errConfiguration
	}
	if _, err := d.Token(); err != io.EOF {
		return result, errConfiguration
	}
	if !coldQuotaCleanupRequiredFields(wire) {
		return result, errConfiguration
	}

	var input coldQuotaCleanupManifestWire
	d = json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || input.Version != 3 || input.Worker.AutomaticCleanup == nil || !*input.Worker.AutomaticCleanup {
		return result, errConfiguration
	}

	// Original bytes have already passed presence, non-null and exact-name
	// checks. Only this private validation copy uses v2 to reuse its automatic
	// mode and common option constraints; neither public parser accepts v3.
	common := input.cleanupManifest
	common.Version = 2
	if common.validate() != nil {
		return result, errConfiguration
	}
	common.Version = 3

	if len(input.Preparation.QuotaSlots) != len(result.Inventory.QuotaSlots) {
		return result, errConfiguration
	}
	copy(result.Inventory.QuotaSlots[:], input.Preparation.QuotaSlots)
	if result.Inventory.Validate() != nil {
		return coldQuotaCleanupManifest{}, errConfiguration
	}

	result.Cleanup = common
	return result, nil
}

// Validate the original shape before projecting common options through v2.
// Every common top-level field is mandatory here; the parser separately requires
// the legacy optional automatic_cleanup field to be true. Recursive checks retain
// exact nested fields and reject null counts, which encoding/json otherwise
// silently converts to zero unsigned integers.
func coldQuotaCleanupRequiredFields(wire []byte) bool {
	var fields map[string]json.RawMessage
	shape := reflect.TypeOf(cleanupManifest{})
	if json.Unmarshal(wire, &fields) != nil || len(fields) != shape.NumField()+1 {
		return false
	}

	for n := 0; n < shape.NumField(); n++ {
		field := shape.Field(n)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		value, present := fields[name]
		if !present || !cleanupRequiredFields(value, field.Type) {
			return false
		}
	}

	profile, present := fields["preparation"]
	return present && cleanupRequiredFields(profile, reflect.TypeOf(coldQuotaCleanupProfile{}))
}
