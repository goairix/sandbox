package runtime

import "time"

// SandboxInfo holds runtime-level sandbox information.
type SandboxInfo struct {
	ID        string
	RuntimeID string // container ID or pod name
	State     string
	CreatedAt time.Time
}

// ExecRequest holds parameters for executing a command in a sandbox.
type ExecRequest struct {
	Command string            // the command to run
	Stdin   string            // optional stdin input
	Timeout int               // seconds
	Env     map[string]string // additional environment variables
	WorkDir string            // working directory, defaults to /workspace
}

// ExecResult holds the result of a synchronous command execution.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// StreamEvent represents a single event in a streamed execution.
type StreamEvent struct {
	Type    StreamEventType
	Content string
}

// StreamEventType is the type of stream event.
type StreamEventType string

const (
	StreamStdout StreamEventType = "stdout"
	StreamStderr StreamEventType = "stderr"
	StreamDone   StreamEventType = "done"
	StreamError  StreamEventType = "error"
)

// SandboxSpec defines what the runtime needs to create a sandbox.
type SandboxSpec struct {
	ID       string
	Image    string
	Memory   string // e.g. "256Mi"
	CPU      string // e.g. "0.5"
	Disk     string // e.g. "100Mi"
	PidLimit int
	// Network
	NetworkEnabled   bool
	NetworkWhitelist []string
	// Security
	ReadOnlyRootFS bool
	RunAsUser      int64
	SeccompProfile string
	// Labels for identification
	Labels map[string]string
	// Mounts specifies host paths to bind-mount into the container.
	Mounts []Mount
}

// Mount describes a host-to-container bind mount.
type Mount struct {
	HostPath      string // absolute path on the host
	ContainerPath string // absolute path inside the container
	ReadOnly      bool
}
