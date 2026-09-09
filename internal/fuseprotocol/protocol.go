// Package fuseprotocol defines the bounded wire contract shared by sandbox-api,
// the trusted workspace mounter, and the unprivileged workspace probe.
package fuseprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	Version                   = 1
	MaxJSONBytes              = 64 << 10
	MaxCredentialBytes        = 4 << 10
	MounterBinary             = "/usr/local/bin/workspace-mounter"
	ProbeBinary               = "/usr/local/bin/workspace-probe"
	ProbePID1EnvironmentName  = "WORKSPACE_PROBE_INTERNAL_PID1_V1"
	ProbePID1EnvironmentValue = "serve"
	ProbeObjectBasenamePrefix = ".workspace-probe-v1-"
	DockerReaperSocket        = "\x00workspace-mounter-reaper-v1"
)

// DeriveProbeObjectName returns the single reserved object basename used by a
// runtime generation's propagation probe. The domain-separated digest makes
// the key reproducible for termination cleanup without exposing RuntimeUID or
// allowing it to become path syntax.
func DeriveProbeObjectName(runtimeUID string, generation int64) (string, error) {
	if !ValidIdentity(runtimeUID) || generation <= 0 {
		return "", fmt.Errorf("invalid probe object identity")
	}
	digest := sha256.Sum256([]byte("sandbox-workspace-probe-object:v1\x00" + runtimeUID + "\x00" + strconv.FormatInt(generation, 10)))
	return ProbeObjectBasenamePrefix + fmt.Sprintf("%x", digest[:]), nil
}

// IsReservedProbeObjectName recognizes only the exact versioned basename
// shape. Public FUSE file APIs use this to hide/reject the reserved object.
func IsReservedProbeObjectName(name string) bool {
	if len(name) != len(ProbeObjectBasenamePrefix)+sha256.Size*2 || !strings.HasPrefix(name, ProbeObjectBasenamePrefix) {
		return false
	}
	for _, char := range name[len(ProbeObjectBasenamePrefix):] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

type BootstrapConfig struct {
	Version               int    `json:"version"`
	RuntimeUID            string `json:"runtime_uid,omitempty"`
	Provider              string `json:"provider"`
	Bucket                string `json:"bucket"`
	Endpoint              string `json:"endpoint"`
	Region                string `json:"region,omitempty"`
	Profile               string `json:"profile"`
	AccessKeyFile         string `json:"access_key_file"`
	SecretKeyFile         string `json:"secret_key_file"`
	PasswdFile            string `json:"passwd_file"`
	CAFile                string `json:"ca_file,omitempty"`
	CacheDir              string `json:"cache_dir"`
	CacheLimitBytes       int64  `json:"cache_limit_bytes"`
	MountPath             string `json:"mount_path"`
	PoolKey               string `json:"pool_key"`
	MountTimeoutSeconds   int64  `json:"mount_timeout_seconds"`
	FlushTimeoutSeconds   int64  `json:"flush_timeout_seconds"`
	UnmountTimeoutSeconds int64  `json:"unmount_timeout_seconds"`
}

type MounterStatus struct {
	Version         int    `json:"version"`
	State           string `json:"state"`
	RuntimeUID      string `json:"runtime_uid"`
	PoolKey         string `json:"pool_key"`
	MountType       string `json:"mount_type"`
	Generation      int64  `json:"generation"`
	RestartDetected bool   `json:"restart_detected"`
	CacheBytes      int64  `json:"cache_bytes"`
	CacheLimitBytes int64  `json:"cache_limit_bytes"`
	CacheExceeded   bool   `json:"cache_exceeded"`
}

type ControlAck struct {
	Version    int    `json:"version"`
	Accepted   bool   `json:"accepted"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
}

// MountCredentials are transported only in the private authorize request.
// They must be cleared by every receiver after the mount attempt and must
// never be copied into durable authorization state.
type MountCredentials struct {
	AccessKey []byte `json:"access_key"`
	SecretKey []byte `json:"secret_key"`
}

func (credentials *MountCredentials) Zero() {
	if credentials == nil {
		return
	}
	for index := range credentials.AccessKey {
		credentials.AccessKey[index] = 0
	}
	for index := range credentials.SecretKey {
		credentials.SecretKey[index] = 0
	}
	credentials.AccessKey = nil
	credentials.SecretKey = nil
}

type AuthorizeRequest struct {
	Version         int              `json:"version"`
	RuntimeUID      string           `json:"runtime_uid"`
	PoolKey         string           `json:"pool_key"`
	WorkspaceHash   string           `json:"workspace_hash"`
	Prefix          string           `json:"prefix"`
	LeaseGeneration int64            `json:"lease_generation"`
	MountAttempt    uint8            `json:"mount_attempt"`
	Credentials     MountCredentials `json:"credentials"`
}

// Sanitized returns the durable authorization identity without credentials.
func (request AuthorizeRequest) Sanitized() AuthorizeRequest {
	request.Credentials = MountCredentials{}
	return request
}

type ControlRequest struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
}

type ProbeResumeRequest struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
	Token      string `json:"token"`
}

type ShutdownAck struct {
	Version         int    `json:"version"`
	RuntimeUID      string `json:"runtime_uid"`
	Generation      int64  `json:"generation"`
	GracefulUnmount bool   `json:"graceful_unmount"`
}

type ProbeStatus struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Token      string `json:"token"`
}

// DockerReaperRequest is the fixed local handshake through which the UID-1000
// probe asks the trusted root PID 1 supervisor to reap one exact detached
// broker child. PID/start-time binding prevents PID reuse from widening it
// into wait4(-1).
type DockerReaperRequest struct {
	Version   int    `json:"version"`
	Command   string `json:"command"`
	PID       int    `json:"pid"`
	UID       int    `json:"uid"`
	StartTime uint64 `json:"start_time"`
}

type DockerReaperResponse struct {
	Version   int    `json:"version"`
	Accepted  bool   `json:"accepted"`
	ErrorCode string `json:"error_code"`
}

// SocketRequest and SocketResponse are the root-only, length-framed local
// transport used between short-lived CLI invocations and the PID 1 supervisor.
// RawMessage fields keep the complete frame within MaxJSONBytes; dispatchers
// must decode Input again into the exact command schema.
type SocketRequest struct {
	Version int             `json:"version"`
	Command string          `json:"command"`
	Input   json.RawMessage `json:"input"`
}

type SocketResponse struct {
	Version   int             `json:"version"`
	OK        bool            `json:"ok"`
	Output    json.RawMessage `json:"output"`
	ErrorCode string          `json:"error_code"`
}

// Decode rejects oversized, unknown, duplicate, null, malformed, and trailing
// JSON while allowing fields explicitly tagged omitempty to be absent.
func Decode(raw []byte, out any) error { return decode(raw, out, false) }

// DecodeExact additionally requires every schema field to be present.
func DecodeExact(raw []byte, out any) error { return decode(raw, out, true) }

func decode(raw []byte, out any, exact bool) error {
	if len(raw) == 0 || len(raw) > MaxJSONBytes || !utf8.Valid(raw) {
		return fmt.Errorf("workspace control JSON has invalid size")
	}
	typ := reflect.TypeOf(out)
	if typ == nil || typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("workspace control decoder requires a struct pointer")
	}
	expected := make(map[string]bool, typ.Elem().NumField())
	for i := 0; i < typ.Elem().NumField(); i++ {
		tag := typ.Elem().Field(i).Tag.Get("json")
		parts := strings.Split(tag, ",")
		if parts[0] == "" || parts[0] == "-" {
			continue
		}
		optional := len(parts) > 1 && parts[1] == "omitempty"
		expected[parts[0]] = optional
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("workspace control JSON must be one object")
	}
	seen := make(map[string]struct{}, len(expected))
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok {
			return fmt.Errorf("workspace control JSON contains an invalid field")
		}
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("workspace control JSON contains an unknown field")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("workspace control JSON contains a duplicate field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("workspace control JSON contains an invalid value")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("workspace control JSON contains a null field")
		}
		seen[key] = struct{}{}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return fmt.Errorf("workspace control JSON is malformed")
	}
	for name, optional := range expected {
		if _, ok := seen[name]; !ok && (exact || !optional) {
			return fmt.Errorf("workspace control JSON is missing a field")
		}
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return fmt.Errorf("workspace control JSON contains trailing data")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("workspace control JSON has invalid field types")
	}
	return nil
}

func AllowedMounterCommand(argv []string) bool {
	if len(argv) == 2 && argv[0] == MounterBinary {
		switch argv[1] {
		case "authorize", "flush", "shutdown":
			return true
		}
	}
	return len(argv) == 3 && argv[0] == MounterBinary && argv[1] == "health" && (argv[2] == "prepared" || argv[2] == "ready")
}

// AllowedMounterCLI is the complete documented public grammar of the trusted
// binary. Callers with a narrower trust boundary must use the corresponding
// runtime-specific allowlist instead.
func AllowedMounterCLI(argv []string) bool {
	if AllowedMounterCommand(argv) {
		return true
	}
	if len(argv) == 2 && argv[0] == MounterBinary && (argv[1] == "supervise" || argv[1] == "bootstrap") {
		return true
	}
	return len(argv) == 4 && argv[0] == MounterBinary && argv[1] == "health" && argv[2] == "prepared" && (argv[3] == "--self-check-image" || argv[3] == "--release-check-image")
}

// AllowedDockerMounterCommand is the private Docker exec surface. The
// container entrypoint owns supervise; bootstrap is additionally required
// because the immutable container ID is learned after ContainerCreate.
func AllowedDockerMounterCommand(argv []string) bool {
	return AllowedMounterCommand(argv) || (len(argv) == 2 && argv[0] == MounterBinary && argv[1] == "bootstrap")
}

func AllowedProbeCommand(argv []string) bool {
	if len(argv) != 6 && len(argv) != 7 {
		return false
	}
	if argv[0] != ProbeBinary || (argv[1] != "write-read-delete" && argv[1] != "quiesce" && argv[1] != "resume") || argv[2] != "--runtime-uid" || !ValidIdentity(argv[3]) || argv[4] != "--generation" {
		return false
	}
	generation, err := strconv.ParseInt(argv[5], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != argv[5] {
		return false
	}
	if argv[1] == "resume" {
		return len(argv) == 7 && argv[6] == "--token-stdin"
	}
	return len(argv) == 6
}

func AllowedProbeCLI(argv []string) bool {
	return AllowedProbeCommand(argv) || (len(argv) == 2 && argv[0] == ProbeBinary && argv[1] == "self-check")
}

func ValidIdentity(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char == 0 || unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func ValidCanonicalPrefix(prefix string) bool {
	if prefix == "" || len(prefix) > 1024 || !utf8.ValidString(prefix) || !strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, "//") || strings.HasPrefix(prefix, "/") {
		return false
	}
	for _, char := range prefix {
		if char == 0 || unicode.IsControl(char) {
			return false
		}
	}
	segments := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || (i == 0 && segment == ".sandbox-system") {
			return false
		}
	}
	return true
}
