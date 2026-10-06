package controlprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const executionDescriptorDomain = "sandbox-exec-descriptor:v1\x00"

// ExecutionRequest contains the fully resolved values that will be executed.
// Callers must supply the complete child environment and explicit credentials,
// working directory, and timeout; the descriptor does not apply defaults.
type ExecutionRequest struct {
	Argv                 []string
	Env                  map[string]string
	UID, GID             uint32
	WorkDir              string
	TimeoutSeconds       uint32
	Stdin                []byte
	TTY, RequiresNetwork bool
}

// ExecutionDescriptor is immutable evidence binding an actual execution payload.
// Its zero value has no evidence. A descriptor alone does not authorize execution.
type ExecutionDescriptor struct {
	digest    string
	canonical []byte
	request   ExecutionRequest
}

type executionEnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type executionMetadata struct {
	Version         int                         `json:"version"`
	Kind            string                      `json:"kind"`
	Argv            []string                    `json:"argv"`
	Env             []executionEnvironmentEntry `json:"env"`
	UID             uint32                      `json:"uid"`
	GID             uint32                      `json:"gid"`
	WorkDir         string                      `json:"work_dir"`
	TimeoutSeconds  uint32                      `json:"timeout_seconds"`
	StdinLength     int64                       `json:"stdin_length"`
	StdinDigest     string                      `json:"stdin_digest"`
	TTY             bool                        `json:"tty"`
	RequiresNetwork bool                        `json:"requires_network"`
}

// NewExecutionDescriptor validates and copies the payload, then binds its
// canonical metadata with a domain-separated SHA-256 digest. Stdin is binary
// and contributes its actual length and digest without being embedded in JSON.
func NewExecutionDescriptor(request ExecutionRequest) (ExecutionDescriptor, error) {
	if err := validateExecutionRequest(request); err != nil {
		return ExecutionDescriptor{}, err
	}
	request = copyExecutionRequest(request)
	env := make([]executionEnvironmentEntry, 0, len(request.Env))
	for name, value := range request.Env {
		env = append(env, executionEnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })
	canonical, err := json.Marshal(executionMetadata{
		Version: 1, Kind: "exec", Argv: request.Argv, Env: env,
		UID: request.UID, GID: request.GID, WorkDir: request.WorkDir,
		TimeoutSeconds: request.TimeoutSeconds, StdinLength: int64(len(request.Stdin)),
		StdinDigest: wireDigest(request.Stdin), TTY: request.TTY, RequiresNetwork: request.RequiresNetwork,
	})
	if err != nil {
		return ExecutionDescriptor{}, fmt.Errorf("encode execution metadata: %w", err)
	}
	if len(canonical) > 65536 {
		return ExecutionDescriptor{}, fmt.Errorf("execution metadata exceeds 65536 bytes")
	}
	hash := sha256.New()
	hash.Write([]byte(executionDescriptorDomain))
	hash.Write(canonical)
	return ExecutionDescriptor{digest: hex.EncodeToString(hash.Sum(nil)), canonical: canonical, request: request}, nil
}

// Digest returns the immutable lowercase SHA-256 evidence, or empty for zero.
func (d ExecutionDescriptor) Digest() string { return d.digest }

// Canonical returns a fresh copy of the canonical metadata, excluding stdin.
func (d ExecutionDescriptor) Canonical() []byte { return append([]byte(nil), d.canonical...) }

// Request returns a fresh copy of all execution values, including binary stdin.
func (d ExecutionDescriptor) Request() ExecutionRequest { return copyExecutionRequest(d.request) }

func copyExecutionRequest(request ExecutionRequest) ExecutionRequest {
	if request.Argv != nil {
		argv := make([]string, len(request.Argv))
		copy(argv, request.Argv)
		request.Argv = argv
	}
	if request.Env != nil {
		env := make(map[string]string, len(request.Env))
		for key, value := range request.Env {
			env[key] = value
		}
		request.Env = env
	}
	if request.Stdin != nil {
		stdin := make([]byte, len(request.Stdin))
		copy(stdin, request.Stdin)
		request.Stdin = stdin
	}
	return request
}

func validateExecutionRequest(request ExecutionRequest) error {
	if len(request.Argv) < 1 || len(request.Argv) > 256 || request.Argv[0] == "" {
		return fmt.Errorf("execution argv requires 1..256 items and a nonempty first item")
	}
	for i, arg := range request.Argv {
		if !validExecutionString(arg, 65536) {
			return fmt.Errorf("invalid execution argv item %d", i)
		}
	}
	if len(request.Env) > 256 {
		return fmt.Errorf("execution env exceeds 256 entries")
	}
	for key, value := range request.Env {
		if !validExecutionEnvironmentKey(key) || !validExecutionString(value, 65536) {
			return fmt.Errorf("invalid execution environment entry")
		}
	}
	if request.UID < 1 || request.UID > 2147483647 || request.GID < 1 || request.GID > 2147483647 {
		return fmt.Errorf("execution uid and gid must each be 1..2147483647")
	}
	if request.WorkDir == "" || !validExecutionString(request.WorkDir, 4096) || !path.IsAbs(request.WorkDir) || path.Clean(request.WorkDir) != request.WorkDir {
		return fmt.Errorf("execution work directory must be a clean absolute POSIX path within 4096 bytes")
	}
	if request.TimeoutSeconds < 1 || request.TimeoutSeconds > 3600 {
		return fmt.Errorf("execution timeout must be 1..3600 seconds")
	}
	if len(request.Stdin) > 1048576 {
		return fmt.Errorf("execution stdin exceeds 1048576 bytes")
	}
	return nil
}

func validExecutionString(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validExecutionEnvironmentKey(key string) bool {
	if len(key) < 1 || len(key) > 256 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
