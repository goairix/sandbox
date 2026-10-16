package deployment

import (
	"bytes"
	"net"
	"strings"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
)

type httpManifest struct {
	Host                  string            `json:"host"`
	Port                  int               `json:"port"`
	APIKeyFile            string            `json:"api_key_file"`
	Env                   map[string]string `json:"env"`
	ExecTimeoutSeconds    int               `json:"exec_timeout_seconds,omitempty"`
	MaxExecTimeoutSeconds int               `json:"max_exec_timeout_seconds,omitempty"`
	MaxActive             int               `json:"max_active,omitempty"`
	RequestsPerSecond     int               `json:"requests_per_second,omitempty"`
}

// HTTPSettings owns mounted key and execution environment copies. Call Zero
// after the gateway has joined; neither field is inherited from the API process.
type HTTPSettings struct {
	Host                  string
	Port                  int
	APIKey                []byte
	Env                   map[string]string
	ExecTimeoutSeconds    int
	MaxExecTimeoutSeconds int
	MaxActive             int
	RequestsPerSecond     int
	UID, GID              uint32
	NetworkAllowed        bool
}

func (s *HTTPSettings) Zero() {
	if s == nil {
		return
	}
	clear(s.APIKey)
	clear(s.Env)
}

func (s *HTTPSettings) clone() *HTTPSettings {
	if s == nil {
		return nil
	}
	owned := *s
	owned.APIKey = bytes.Clone(s.APIKey)
	owned.Env = cloneHTTPEnvironment(s.Env)
	return &owned
}

func cloneHTTPEnvironment(env map[string]string) map[string]string {
	owned := make(map[string]string, len(env))
	for name, value := range env {
		owned[strings.Clone(name)] = strings.Clone(value)
	}
	return owned
}

func loadHTTPSettings(base string, input *httpManifest, launcher launcherconfig.Config) (*HTTPSettings, error) {
	if input == nil {
		return nil, nil
	}
	if net.ParseIP(input.Host) == nil || input.Port < 1 || input.Port > 65535 || input.APIKeyFile == "" || input.Env == nil {
		return nil, fileError("HTTP settings")
	}
	s := &HTTPSettings{Host: strings.Clone(input.Host), Port: input.Port, Env: cloneHTTPEnvironment(input.Env), ExecTimeoutSeconds: input.ExecTimeoutSeconds, MaxExecTimeoutSeconds: input.MaxExecTimeoutSeconds, MaxActive: input.MaxActive, RequestsPerSecond: input.RequestsPerSecond, UID: launcher.UID, GID: launcher.GID, NetworkAllowed: launcher.NetworkAllowed}
	if s.ExecTimeoutSeconds == 0 {
		s.ExecTimeoutSeconds = 30
	}
	if s.MaxExecTimeoutSeconds == 0 {
		s.MaxExecTimeoutSeconds = 600
	}
	if s.MaxActive == 0 {
		s.MaxActive = 32
	}
	if s.RequestsPerSecond == 0 {
		s.RequestsPerSecond = 100
	}
	if s.ExecTimeoutSeconds < 1 || s.ExecTimeoutSeconds > s.MaxExecTimeoutSeconds || s.MaxExecTimeoutSeconds > 3600 || s.MaxActive < 1 || s.MaxActive > 32 || s.RequestsPerSecond < 1 || s.RequestsPerSecond > 10000 {
		s.Zero()
		return nil, fileError("HTTP budgets")
	}
	// Use the original descriptor's exact environment and credential validation.
	if _, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/bin/sh", "-c", ""}, Env: s.Env, UID: s.UID, GID: s.GID, WorkDir: "/workspace", TimeoutSeconds: uint32(s.ExecTimeoutSeconds)}); err != nil {
		s.Zero()
		return nil, fileError("HTTP execution profile")
	}
	wire, err := boundedFile(relativeFile(base, input.APIKeyFile), 4096, "HTTP API key")
	if err != nil {
		s.Zero()
		return nil, err
	}
	defer clear(wire)
	key := bytes.Trim(wire, " \t\r\n\v\f")
	if len(key) == 0 {
		s.Zero()
		return nil, fileError("HTTP API key")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			s.Zero()
			return nil, fileError("HTTP API key")
		}
	}
	s.APIKey = bytes.Clone(key)
	return s, nil
}
