package provisioning

import (
	"bytes"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/resource"
)

// decodeColdCapsule validates both historical formats before the caller uses
// their attribution or business configuration. No deployment defaults are used.
func decodeColdCapsule(wire []byte, capsule *plainColdCapsule) error {
	if capsule == nil || len(wire) > 65536 || !utf8.Valid(wire) {
		return ErrPlainColdProfile
	}
	var decoded plainColdCapsule
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return ErrPlainColdProfile
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(canonical, wire) || !coldDigest(decoded.Deployment) || !coldDigest(decoded.WorkspaceHash) || decoded.TTL <= 0 || decoded.TTL > 365*24*time.Hour {
		return ErrPlainColdProfile
	}
	for _, id := range []string{decoded.RequestID, decoded.IntentID, decoded.SandboxID, decoded.SnapshotVersion} {
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
			return ErrPlainColdProfile
		}
	}
	switch decoded.Version {
	case 1:
		if len(decoded.Business) != 0 {
			return ErrPlainColdProfile
		}
	case 2:
		if _, err := sandboxsnapshot.Decode(decoded.Business); err != nil {
			return ErrPlainColdProfile
		}
	default:
		return ErrPlainColdProfile
	}
	*capsule = decoded
	return nil
}

func (s *PlainColdService) validateColdBusiness(capsule plainColdCapsule) error {
	if capsule.Version == 1 && len(capsule.Business) == 0 {
		return nil
	}
	if capsule.Version != 2 {
		return ErrPlainColdProfile
	}
	business, err := sandboxsnapshot.Decode(capsule.Business)
	if err != nil || business.Resolved.TTL.Kind != "finite" || time.Duration(business.Resolved.TTL.Seconds)*time.Second != capsule.TTL {
		return ErrPlainColdProfile
	}
	requested := business.Requested
	if requested.WorkspacePath != "" || requested.WorkspaceMountMode != "" || business.Resolved.WorkspaceMountMode != "" || len(requested.WorkspaceSyncExclude) != 0 || len(requested.Dependencies) != 0 {
		return ErrPlainColdProfile
	}
	enabled := false
	if requested.Network != nil {
		enabled = requested.Network.Enabled
		if len(requested.Network.Whitelist) != 0 || requested.Network.BlockPrivate {
			return ErrPlainColdProfile
		}
	}
	if enabled != s.config.NetworkAllowed {
		return ErrPlainColdProfile
	}
	tmp := s.spec.TmpDisk
	if tmp == "" {
		tmp = runtime.DefaultTmpDisk
	}
	for _, pair := range [][2]string{{business.Resolved.Resources.CPU, s.spec.CPU}, {business.Resolved.Resources.Memory, s.spec.Memory}, {business.Resolved.Resources.Disk, s.spec.Disk}, {business.Resolved.Resources.TmpDisk, tmp}} {
		resolved, err := resource.ParseQuantity(pair[0])
		if err != nil {
			return ErrPlainColdProfile
		}
		actual, err := resource.ParseQuantity(pair[1])
		if err != nil || resolved.Cmp(actual) != 0 {
			return ErrPlainColdProfile
		}
	}
	return nil
}
