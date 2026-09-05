package runtime

import "time"

// SandboxInfo holds runtime-level sandbox information.
type SandboxInfo struct {
	ID         string
	RuntimeID  string // container ID or pod name
	RuntimeUID string // immutable container ID or pod UID
	State      string
	CreatedAt  time.Time
}

// ExecRequest holds parameters for executing a command in a sandbox.
type ExecRequest struct {
	Command         string            // the command to run
	Stdin           string            // optional stdin input
	Timeout         int               // seconds
	Env             map[string]string // additional environment variables
	WorkDir         string            // working directory, defaults to /workspace
	LineBuffered    bool              // allocate TTY for real-time output
	RequiresNetwork bool              // if true, caller asserts this command needs network
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
	StreamPing   StreamEventType = "ping" // keepalive heartbeat
)

// SandboxSpec defines what the runtime needs to create a sandbox.
type SandboxSpec struct {
	ID            string
	Image         string
	Memory        string // limit e.g. "512Mi"
	MemoryRequest string // request e.g. "128Mi"; defaults to Memory when empty
	CPU           string // limit e.g. "500m"
	CPURequest    string // request e.g. "100m"; defaults to CPU when empty
	Disk          string // /workspace, e.g. "100Mi"
	TmpDisk       string // /tmp, e.g. "50Mi"; defaults to DefaultTmpDisk
	PidLimit      int
	// Network
	NetworkEnabled      bool
	NetworkWhitelist    []string
	NetworkBlockPrivate bool
	// Security
	ReadOnlyRootFS bool
	RunAsUser      int64
	SeccompProfile string
	// Labels for identification
	Labels map[string]string
	// Mounts specifies host paths to bind-mount into the container.
	Mounts []Mount
	// WorkspaceFUSE contains the fixed, prefix-free configuration used to prepare
	// a FUSE-backed sandbox. Per-workspace authorization is supplied separately.
	WorkspaceFUSE *WorkspaceFUSESpec
}

// WorkspaceFUSESpec contains only configuration fixed at sandbox preparation
// time. It deliberately excludes workspace prefixes and lease generations.
type WorkspaceFUSESpec struct {
	RuntimeType     string
	Provider        string
	Driver          string
	Profile         string
	StorageIdentity string
	// CredentialGeneration is a non-secret operator-maintained version. It is
	// part of the pool identity so credential rotations cannot reuse old shells.
	CredentialGeneration string
	MounterImage         string
	// DockerImage is the digest-pinned special sandbox image that contains the
	// trusted PID 1 supervisor, s3fs and the unprivileged workspace probe.
	DockerImage      string
	SecretName       string
	CASecretKey      string
	EndpointHostIPs  []string
	Bucket           string
	Endpoint         string
	Region           string
	UseSSL           bool
	CacheSize        string
	CacheMedium      string
	MountTimeout     time.Duration
	FlushTimeout     time.Duration
	UnmountTimeout   time.Duration
	LSMProfile       string
	MounterResources WorkspaceFUSEResources
	SystemEgress     SystemEgressSpec
	PoolKey          string
}

// WorkspaceFUSEResources contains the fixed resource requests and limits used
// by the trusted mounter. Runtime renderers consume these values directly from
// the prepared spec rather than consulting mutable global configuration.
type WorkspaceFUSEResources struct {
	CPURequest              string
	CPULimit                string
	MemoryRequest           string
	MemoryLimit             string
	EphemeralStorageRequest string
	EphemeralStorageLimit   string
}

// SystemEgressMode selects the runtime-specific system egress policy shape.
type SystemEgressMode string

const (
	SystemEgressCIDR       SystemEgressMode = "cidr"
	SystemEgressCiliumFQDN SystemEgressMode = "cilium-fqdn"
)

// SystemEgressSpec describes the fixed DNS and object-store destinations that
// the trusted mounter may reach. ProxyURL is reserved and must remain empty in
// phase one.
type SystemEgressSpec struct {
	Mode          SystemEgressMode
	DNSCIDRs      []string
	DNSPorts      []int32
	EndpointCIDRs []string
	EndpointFQDNs []string
	EndpointPorts []int32
	ProxyURL      string
}

// WorkspaceMountAuthorization is the one-shot, lease-bound authorization that
// binds a prepared sandbox to exactly one workspace prefix.
type WorkspaceMountAuthorization struct {
	RuntimeUID      string
	PoolKey         string
	WorkspaceHash   string
	Prefix          string
	LeaseGeneration int64
	MountAttempt    uint8
}

// WorkspaceHealth reports trusted mounter health for the mounted workspace.
type WorkspaceHealth struct {
	Ready           bool
	MountType       string
	RuntimeUID      string
	Generation      int64
	RestartCount    int32
	RestartDetected bool
	CacheBytes      int64
	CacheLimitBytes int64
	CacheExceeded   bool
	LastSuccessful  time.Time
	Error           string
}

// WorkspaceQuiesceToken binds a single resume operation to an exact runtime
// generation. Runtime implementations must reject stale, replayed, and
// cross-runtime tokens.
type WorkspaceQuiesceToken struct {
	RuntimeUID string
	Generation int64
	Opaque     string
}

// TerminationEvidence records trusted evidence that an exact runtime instance
// can no longer access its workspace.
type TerminationEvidence struct {
	RuntimeUID           string
	NodeName             string
	GracefulUnmount      bool
	ProcessExited        bool
	InfrastructureFenced bool
}

const DefaultTmpDisk = "50Mi"

// Mount describes a host-to-container bind mount.
type Mount struct {
	HostPath      string // absolute path on the host
	ContainerPath string // absolute path inside the container
	ReadOnly      bool
}
