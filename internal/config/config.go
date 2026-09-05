package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"
	"github.com/joho/godotenv"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/goairix/sandbox/internal/mounter"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
)

const (
	VarPath  string = "var"
	LogPath         = VarPath + "/logs"
	TempPath        = VarPath + "/tmp"
)

var canonicalFUSERegion = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Config is the root configuration structure for the sandbox service.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Runtime   RuntimeConfig   `mapstructure:"runtime"`
	Pool      PoolConfig      `mapstructure:"pool"`
	Images    ImagesConfig    `mapstructure:"images"`
	Storage   StorageConfig   `mapstructure:"storage"`
	Workspace WorkspaceConfig `mapstructure:"workspace"`
	Security  SecurityConfig  `mapstructure:"security"`
	Telemetry Telemetry       `mapstructure:"telemetry"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int    `mapstructure:"port"`
	Host string `mapstructure:"host"`
}

// RuntimeConfig holds sandbox runtime settings.
type RuntimeConfig struct {
	Type       string           `mapstructure:"type"`
	Docker     DockerConfig     `mapstructure:"docker"`
	Kubernetes KubernetesConfig `mapstructure:"kubernetes"`
}

// DockerConfig holds Docker-specific runtime settings.
type DockerConfig struct {
	Host                string `mapstructure:"host"`
	WorkspaceSecretRoot string `mapstructure:"workspace_secret_root"`
}

// KubernetesConfig holds Kubernetes-specific runtime settings.
type KubernetesConfig struct {
	Kubeconfig string `mapstructure:"kubeconfig"`
	Namespace  string `mapstructure:"namespace"`
}

// PoolConfig holds sandbox pool settings.
type PoolConfig struct {
	MinSize               int `mapstructure:"min_size"`
	MaxSize               int `mapstructure:"max_size"`
	RefillIntervalSeconds int `mapstructure:"refill_interval_seconds"`
}

// ImagesConfig holds sandbox container image settings.
type ImagesConfig struct {
	Sandbox string `mapstructure:"sandbox"`
	Gateway string `mapstructure:"gateway"`
}

// StorageConfig holds storage backend settings.
type StorageConfig struct {
	State      StateStorageConfig `mapstructure:"state"`
	FileSystem FileSystemConfig   `mapstructure:"filesystem"`
}

// StateStorageConfig holds state storage settings.
type StateStorageConfig struct {
	Redis RedisConfig `mapstructure:"redis"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// FileSystemConfig holds filesystem storage settings.
// Provider can be one of: local, s3, cos, oss, obs, minio.
type FileSystemConfig struct {
	Provider         string                         `mapstructure:"provider"`
	Bucket           string                         `mapstructure:"bucket"`
	Region           string                         `mapstructure:"region"`
	Endpoint         string                         `mapstructure:"endpoint"`
	AccessKey        string                         `mapstructure:"access_key"`
	SecretKey        string                         `mapstructure:"secret_key"`
	SessionToken     string                         `mapstructure:"session_token"`
	CredentialExpiry string                         `mapstructure:"credential_expiry"`
	CredentialFiles  FileSystemCredentialFileConfig `mapstructure:"credential_files"`
	CAFile           string                         `mapstructure:"ca_file"`
	LocalPath        string                         `mapstructure:"local_path"`
	SubPath          string                         `mapstructure:"sub_path"`
	UseSSL           bool                           `mapstructure:"use_ssl"`
}

// FileSystemCredentialFileConfig holds paths to credentials mounted from a
// trusted external secret source.
type FileSystemCredentialFileConfig struct {
	AccessKeyFile        string `mapstructure:"access_key_file"`
	SecretKeyFile        string `mapstructure:"secret_key_file"`
	SessionTokenFile     string `mapstructure:"session_token_file"`
	CredentialExpiryFile string `mapstructure:"credential_expiry_file"`
}

// WorkspaceFUSEProviderConfig holds an approved provider-specific FUSE
// profile. The profile is selected by storage.filesystem.provider.
type WorkspaceFUSEProviderConfig struct {
	Driver               string   `mapstructure:"driver"`
	Profile              string   `mapstructure:"profile"`
	StorageIdentity      string   `mapstructure:"storage_identity"`
	MounterImage         string   `mapstructure:"mounter_image"`
	DockerImage          string   `mapstructure:"docker_image"`
	CASecretKey          string   `mapstructure:"ca_secret_key"`
	CredentialGeneration string   `mapstructure:"credential_generation"`
	EndpointHostIPs      []string `mapstructure:"endpoint_host_ips"`
	LSMProfile           string   `mapstructure:"lsm_profile"`
	SystemEgressMode     string   `mapstructure:"system_egress_mode"`
	DNSCIDRs             []string `mapstructure:"dns_cidrs"`
	SystemEgressFQDNs    []string `mapstructure:"system_egress_fqdns"`
	SystemEgressCIDRs    []string `mapstructure:"system_egress_cidrs"`
	EndpointPorts        []int32  `mapstructure:"endpoint_ports"`
	ProxyURL             string   `mapstructure:"proxy_url"`
}

// WorkspaceFUSEResourceConfig holds mounter sidecar/container resource
// requests and limits.
type WorkspaceFUSEResourceConfig struct {
	CPURequest              string `mapstructure:"cpu_request"`
	CPULimit                string `mapstructure:"cpu_limit"`
	MemoryRequest           string `mapstructure:"memory_request"`
	MemoryLimit             string `mapstructure:"memory_limit"`
	EphemeralStorageRequest string `mapstructure:"ephemeral_storage_request"`
	EphemeralStorageLimit   string `mapstructure:"ephemeral_storage_limit"`
}

// WorkspaceFUSEPoolConfig holds settings for the dedicated FUSE runtime pool.
type WorkspaceFUSEPoolConfig struct {
	MinSize               int `mapstructure:"min_size"`
	MaxSize               int `mapstructure:"max_size"`
	RefillIntervalSeconds int `mapstructure:"refill_interval_seconds"`
	PrepareTimeoutSeconds int `mapstructure:"prepare_timeout_seconds"`
}

// WorkspaceConfig holds workspace sync or FUSE strategy settings.
type WorkspaceConfig struct {
	// AutoSyncIntervalSeconds is the interval between automatic sync-from-container
	// cycles. Set to 0 to disable auto-sync. Recommended: 30 for long-running
	// agent sessions.
	AutoSyncIntervalSeconds   int                                    `mapstructure:"auto_sync_interval_seconds"`
	Mode                      string                                 `mapstructure:"mode"`
	SecretName                string                                 `mapstructure:"secret_name"`
	CacheSize                 string                                 `mapstructure:"cache_size"`
	CacheMedium               string                                 `mapstructure:"cache_medium"`
	MountTimeoutSeconds       int                                    `mapstructure:"mount_timeout_seconds"`
	FlushTimeoutSeconds       int                                    `mapstructure:"flush_timeout_seconds"`
	UnmountTimeoutSeconds     int                                    `mapstructure:"unmount_timeout_seconds"`
	RecreateMaxAttempts       int                                    `mapstructure:"recreate_max_attempts"`
	LeaseTTLSeconds           int                                    `mapstructure:"lease_ttl_seconds"`
	LeaseRenewIntervalSeconds int                                    `mapstructure:"lease_renew_interval_seconds"`
	QuotaMode                 string                                 `mapstructure:"quota_mode"`
	MounterResources          WorkspaceFUSEResourceConfig            `mapstructure:"mounter_resources"`
	FUSEPool                  WorkspaceFUSEPoolConfig                `mapstructure:"fuse_pool"`
	Providers                 map[string]WorkspaceFUSEProviderConfig `mapstructure:"providers"`
}

// SecurityConfig holds sandbox security constraints.
type SecurityConfig struct {
	APIKey                string   `mapstructure:"api_key"`
	RateLimit             int      `mapstructure:"rate_limit"` // requests per second, 0 = disabled
	ExecTimeoutSeconds    int      `mapstructure:"exec_timeout_seconds"`
	MaxExecTimeoutSeconds int      `mapstructure:"max_exec_timeout_seconds"`
	SandboxTimeoutSeconds int      `mapstructure:"sandbox_timeout_seconds"`
	MaxMemory             string   `mapstructure:"max_memory"`
	MaxMemoryRequest      string   `mapstructure:"max_memory_request"`
	MaxDisk               string   `mapstructure:"max_disk"`
	MaxTmpDisk            string   `mapstructure:"max_tmp_disk"`
	MaxPids               int      `mapstructure:"max_pids"`
	MaxCPU                string   `mapstructure:"max_cpu"`
	MaxCPURequest         string   `mapstructure:"max_cpu_request"`
	MaxUploadBytes        int64    `mapstructure:"max_upload_bytes"`
	NetworkEnabled        bool     `mapstructure:"network_enabled"`
	NetworkWhitelist      []string `mapstructure:"network_whitelist"`
	SeccompProfile        string   `mapstructure:"seccomp_profile"`
}

// Telemetry 平台监控配置
type Telemetry struct {
	ServiceName    string `mapstructure:"service_name"`
	ServiceVersion string `mapstructure:"service_version"`
	Tracer         otlp   `mapstructure:"tracer"`
	Metrics        otlp   `mapstructure:"metrics"`
	Log            otlp   `mapstructure:"log"`
}

// tracer 链路追踪配置
type otlp struct {
	OtlpEnabled  bool   `mapstructure:"otlp_enabled"`
	OtlpEndpoint string `mapstructure:"otlp_endpoint"`
}

// Load reads configuration from the given file path (if non-empty), applies
// defaults, and overlays any SANDBOX_* environment variables.
//
// Environment variable naming convention:
//
//	SANDBOX_<SECTION>_<KEY>
//
// Examples:
//
//	SANDBOX_SERVER_PORT=9090
//	SANDBOX_STORAGE_STATE_REDIS_ADDR=redis:6379
func Load(path string) (*Config, error) {
	// 加载.env
	_ = godotenv.Load()

	v := viper.New()

	// ------------------------------------------------------------------ defaults
	setDefaults(v)

	// ----------------------------------------------------------------- from file
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("config: read file %q: %w", path, err)
		}
	}

	// --------------------------------------------------------- env var overrides
	v.SetEnvPrefix("SANDBOX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := bindFUSEProviderEnv(v); err != nil {
		return nil, err
	}

	// Explicitly bind every known key so that env vars are honoured even when
	// no config file is present. Using v.AllKeys() (populated by SetDefault
	// and ReadInConfig) avoids maintaining a separate manual key list.
	for _, key := range v.AllKeys() {
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("config: bind env for %q: %w", key, err)
		}
	}

	// ------------------------------------------------------------ unmarshal
	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks the configuration for invalid or missing values.
func (c *Config) Validate() error {
	// Security: api_key
	if c.Security.APIKey == "" {
		return fmt.Errorf("config: security.api_key must not be empty")
	}

	// Pool sizes
	if c.Pool.MinSize < 0 {
		return fmt.Errorf("config: pool.min_size must be >= 0, got %d", c.Pool.MinSize)
	}
	if c.Pool.MaxSize <= 0 {
		return fmt.Errorf("config: pool.max_size must be > 0, got %d", c.Pool.MaxSize)
	}
	if c.Pool.MaxSize < c.Pool.MinSize {
		return fmt.Errorf("config: pool.max_size (%d) must be >= pool.min_size (%d)", c.Pool.MaxSize, c.Pool.MinSize)
	}

	// Server port
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: server.port must be in range 1-65535, got %d", c.Server.Port)
	}

	// Runtime type
	switch c.Runtime.Type {
	case "docker", "kubernetes":
		// valid
	default:
		return fmt.Errorf("config: runtime.type must be \"docker\" or \"kubernetes\", got %q", c.Runtime.Type)
	}

	// Kubernetes namespace
	if c.Runtime.Type == "kubernetes" && c.Runtime.Kubernetes.Namespace == "" {
		return fmt.Errorf("config: runtime.kubernetes.namespace must not be empty when runtime.type is \"kubernetes\"")
	}

	// Exec timeout
	if c.Security.ExecTimeoutSeconds <= 0 {
		return fmt.Errorf("config: security.exec_timeout_seconds must be > 0, got %d", c.Security.ExecTimeoutSeconds)
	}
	if c.Security.MaxExecTimeoutSeconds <= 0 {
		return fmt.Errorf("config: security.max_exec_timeout_seconds must be > 0, got %d", c.Security.MaxExecTimeoutSeconds)
	}
	if c.Security.MaxExecTimeoutSeconds < c.Security.ExecTimeoutSeconds {
		return fmt.Errorf("config: security.max_exec_timeout_seconds (%d) must be >= security.exec_timeout_seconds (%d)", c.Security.MaxExecTimeoutSeconds, c.Security.ExecTimeoutSeconds)
	}
	if c.Security.MaxUploadBytes <= 0 {
		return fmt.Errorf("config: security.max_upload_bytes must be > 0, got %d", c.Security.MaxUploadBytes)
	}

	// Workspace auto-sync
	if c.Workspace.AutoSyncIntervalSeconds < 0 {
		return fmt.Errorf("config: workspace.auto_sync_interval_seconds must be >= 0, got %d", c.Workspace.AutoSyncIntervalSeconds)
	}

	switch c.Workspace.Mode {
	case "sync":
		return nil
	case "fuse":
		return c.validateFUSE()
	default:
		return fmt.Errorf("config: workspace.mode must be \"sync\" or \"fuse\", got %q", c.Workspace.Mode)
	}
}

func (c *Config) validateFUSE() error {
	filesystem := c.Storage.FileSystem
	workspace := c.Workspace
	if c.Runtime.Type == "docker" && (c.Runtime.Docker.WorkspaceSecretRoot == "/" || !filepath.IsAbs(c.Runtime.Docker.WorkspaceSecretRoot) || filepath.Clean(c.Runtime.Docker.WorkspaceSecretRoot) != c.Runtime.Docker.WorkspaceSecretRoot) {
		return fmt.Errorf("config: runtime.docker.workspace_secret_root must be a canonical absolute path when workspace.mode is \"fuse\"")
	}

	if filesystem.Provider != "minio" && filesystem.Provider != "obs" {
		return fmt.Errorf("config: workspace.mode=fuse supports only minio or obs, got %q", filesystem.Provider)
	}
	providerPath := "workspace.providers." + filesystem.Provider
	provider, ok := workspace.Providers[filesystem.Provider]
	if !ok {
		return fmt.Errorf("config: workspace.providers.%s must be configured", filesystem.Provider)
	}
	if c.Storage.State.Redis.Addr == "" {
		return fmt.Errorf("config: storage.state.redis.addr must not be empty when workspace.mode is \"fuse\"")
	}
	if workspace.SecretName == "" {
		return fmt.Errorf("config: workspace.secret_name must not be empty when workspace.mode is \"fuse\"")
	}
	if filesystem.Bucket == "" {
		return fmt.Errorf("config: storage.filesystem.bucket must not be empty when workspace.mode is \"fuse\"")
	}
	if filesystem.Endpoint == "" {
		return fmt.Errorf("config: storage.filesystem.endpoint must not be empty when workspace.mode is \"fuse\"")
	}
	if !filesystem.UseSSL {
		return fmt.Errorf("config: storage.filesystem.use_ssl must be true for TLS-required FUSE profiles")
	}
	switch filesystem.Provider {
	case "obs":
		endpoint, err := url.Parse(filesystem.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
			return fmt.Errorf("config: storage.filesystem.endpoint must be a canonical TLS HTTPS URL for OBS FUSE")
		}
	}
	if filesystem.CredentialFiles.AccessKeyFile == "" {
		return fmt.Errorf("config: storage.filesystem.credential_files.access_key_file must not be empty when workspace.mode is \"fuse\"")
	}
	if filesystem.CredentialFiles.SecretKeyFile == "" {
		return fmt.Errorf("config: storage.filesystem.credential_files.secret_key_file must not be empty when workspace.mode is \"fuse\"")
	}
	if filesystem.AccessKey != "" {
		return fmt.Errorf("config: inline access_key is forbidden when workspace.mode is \"fuse\"")
	}
	if filesystem.SecretKey != "" {
		return fmt.Errorf("config: inline secret_key is forbidden when workspace.mode is \"fuse\"")
	}
	if hasTemporaryCredentialFields(filesystem) {
		return fmt.Errorf("config: session token or credential expiry fields are unsupported when workspace.mode is \"fuse\"")
	}
	if (filesystem.CAFile == "") != (provider.CASecretKey == "") {
		return fmt.Errorf("config: ca_file and ca_secret_key must both be empty or both be configured (storage.filesystem.ca_file, %s.ca_secret_key)", providerPath)
	}

	pool := workspace.FUSEPool
	if pool.MinSize < 0 {
		return fmt.Errorf("config: workspace.fuse_pool.min_size must be >= 0, got %d", pool.MinSize)
	}
	if pool.MaxSize <= 0 {
		return fmt.Errorf("config: workspace.fuse_pool.max_size must be > 0, got %d", pool.MaxSize)
	}
	if pool.MinSize > pool.MaxSize {
		return fmt.Errorf("config: workspace.fuse_pool.max_size (%d) must be >= workspace.fuse_pool.min_size (%d)", pool.MaxSize, pool.MinSize)
	}
	if pool.RefillIntervalSeconds <= 0 {
		return fmt.Errorf("config: workspace.fuse_pool.refill_interval_seconds must be > 0, got %d", pool.RefillIntervalSeconds)
	}
	if pool.PrepareTimeoutSeconds <= 0 {
		return fmt.Errorf("config: workspace.fuse_pool.prepare_timeout_seconds must be > 0, got %d", pool.PrepareTimeoutSeconds)
	}

	if workspace.MountTimeoutSeconds <= 0 {
		return fmt.Errorf("config: workspace.mount_timeout_seconds must be > 0, got %d", workspace.MountTimeoutSeconds)
	}
	if workspace.FlushTimeoutSeconds <= 0 {
		return fmt.Errorf("config: workspace.flush_timeout_seconds must be > 0, got %d", workspace.FlushTimeoutSeconds)
	}
	if workspace.UnmountTimeoutSeconds <= 0 {
		return fmt.Errorf("config: workspace.unmount_timeout_seconds must be > 0, got %d", workspace.UnmountTimeoutSeconds)
	}
	if workspace.LeaseTTLSeconds <= 0 {
		return fmt.Errorf("config: workspace.lease_ttl_seconds must be > 0, got %d", workspace.LeaseTTLSeconds)
	}
	if workspace.LeaseRenewIntervalSeconds <= 0 || workspace.LeaseRenewIntervalSeconds > workspace.LeaseTTLSeconds/3 {
		return fmt.Errorf("config: workspace.lease_renew_interval_seconds must be > 0 and <= workspace.lease_ttl_seconds/3, got %d", workspace.LeaseRenewIntervalSeconds)
	}
	if workspace.RecreateMaxAttempts != 1 {
		return fmt.Errorf("config: workspace.recreate_max_attempts must equal 1, got %d", workspace.RecreateMaxAttempts)
	}
	if workspace.CacheMedium != "disk" {
		return fmt.Errorf("config: workspace.cache_medium must be \"disk\", got %q", workspace.CacheMedium)
	}
	cacheSize, err := resource.ParseQuantity(workspace.CacheSize)
	if err != nil || cacheSize.Sign() <= 0 {
		return fmt.Errorf("config: workspace.cache_size must be a positive Kubernetes quantity, got %q", workspace.CacheSize)
	}
	resources := workspace.MounterResources
	cpuRequest, err := positiveQuantity("workspace.mounter_resources.cpu_request", resources.CPURequest)
	if err != nil {
		return err
	}
	cpuLimit, err := positiveQuantity("workspace.mounter_resources.cpu_limit", resources.CPULimit)
	if err != nil {
		return err
	}
	memoryRequest, err := positiveQuantity("workspace.mounter_resources.memory_request", resources.MemoryRequest)
	if err != nil {
		return err
	}
	memoryLimit, err := positiveQuantity("workspace.mounter_resources.memory_limit", resources.MemoryLimit)
	if err != nil {
		return err
	}
	ephemeralRequest, err := positiveQuantity("workspace.mounter_resources.ephemeral_storage_request", resources.EphemeralStorageRequest)
	if err != nil {
		return err
	}
	ephemeralLimit, err := positiveQuantity("workspace.mounter_resources.ephemeral_storage_limit", resources.EphemeralStorageLimit)
	if err != nil {
		return err
	}
	if cpuRequest.Cmp(cpuLimit) > 0 {
		return fmt.Errorf("config: workspace.mounter_resources.cpu_request must be <= workspace.mounter_resources.cpu_limit")
	}
	if memoryRequest.Cmp(memoryLimit) > 0 {
		return fmt.Errorf("config: workspace.mounter_resources.memory_request must be <= workspace.mounter_resources.memory_limit")
	}
	if ephemeralRequest.Cmp(ephemeralLimit) > 0 {
		return fmt.Errorf("config: workspace.mounter_resources.ephemeral_storage_request must be <= workspace.mounter_resources.ephemeral_storage_limit")
	}
	if ephemeralLimit.Cmp(cacheSize) < 0 {
		return fmt.Errorf("config: workspace.mounter_resources.ephemeral_storage_limit must be >= workspace.cache_size")
	}
	if workspace.QuotaMode != "soft" {
		return fmt.Errorf("config: workspace.quota_mode must be \"soft\", got %q", workspace.QuotaMode)
	}

	if provider.Driver != "s3fs" {
		return fmt.Errorf("config: %s.driver must be \"s3fs\", got %q", providerPath, provider.Driver)
	}
	if provider.Profile == "" {
		return fmt.Errorf("config: %s.profile must not be empty", providerPath)
	}
	if profile, ok := mounter.InspectCompiledProfile(provider.Profile); ok && profile.Provider == filesystem.Provider && profile.Descriptor.RegionOption == "endpoint" && !canonicalFUSERegion.MatchString(filesystem.Region) {
		return fmt.Errorf("config: storage.filesystem.region must be a canonical region for FUSE profile %q", provider.Profile)
	}
	if provider.StorageIdentity == "" {
		return fmt.Errorf("config: %s.storage_identity must not be empty", providerPath)
	}
	if provider.CredentialGeneration == "" {
		return fmt.Errorf("config: %s.credential_generation must not be empty", providerPath)
	}
	if !isDigestPinnedImage(provider.MounterImage) {
		return fmt.Errorf("config: %s.mounter_image must contain a valid @sha256 digest", providerPath)
	}
	if !isDigestPinnedImage(provider.DockerImage) {
		return fmt.Errorf("config: %s.docker_image must contain a valid @sha256 digest", providerPath)
	}
	lsmProfile := strings.ToLower(strings.TrimSpace(provider.LSMProfile))
	if lsmProfile == "" || lsmProfile == "unconfined" || lsmProfile == "label=disable" {
		return fmt.Errorf("config: %s.lsm_profile must be a confined profile", providerPath)
	}
	if provider.SystemEgressMode != "cidr" && provider.SystemEgressMode != "cilium-fqdn" {
		return fmt.Errorf("config: %s.system_egress_mode must be \"cidr\" or \"cilium-fqdn\", got %q", providerPath, provider.SystemEgressMode)
	}
	if len(provider.DNSCIDRs) == 0 {
		return fmt.Errorf("config: %s.dns_cidrs must not be empty", providerPath)
	}
	for _, cidr := range provider.DNSCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.String() != cidr || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
			return fmt.Errorf("config: %s.dns_cidrs must contain host-only /32 or /128 CIDRs, got %q", providerPath, cidr)
		}
		if prefix.Bits() != prefix.Addr().BitLen() {
			return fmt.Errorf("config: %s.dns_cidrs must contain host-only /32 or /128 CIDRs, got %q", providerPath, cidr)
		}
		if !sandboxruntime.IsPublicDNSAddress(prefix.Addr()) {
			return fmt.Errorf("config: %s.dns_cidrs must contain public resolver addresses, got %q", providerPath, cidr)
		}
	}
	if len(provider.EndpointPorts) == 0 {
		return fmt.Errorf("config: %s.endpoint_ports must not be empty", providerPath)
	}
	for _, port := range provider.EndpointPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("config: %s.endpoint_ports must be in range 1-65535, got %d", providerPath, port)
		}
	}
	if provider.ProxyURL != "" {
		return fmt.Errorf("config: %s.proxy_url must be empty", providerPath)
	}

	approvedNetworks := make([]*net.IPNet, 0, len(provider.SystemEgressCIDRs))
	for _, cidr := range provider.SystemEgressCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("config: %s.system_egress_cidrs contains invalid CIDR %q", providerPath, cidr)
		}
		approvedNetworks = append(approvedNetworks, network)
	}
	if provider.SystemEgressMode == "cidr" && len(approvedNetworks) == 0 {
		return fmt.Errorf("config: %s.system_egress_cidrs must not be empty in cidr mode", providerPath)
	}
	for _, rawIP := range provider.EndpointHostIPs {
		ip := net.ParseIP(rawIP)
		if ip == nil {
			return fmt.Errorf("config: %s.endpoint_host_ips must contain literal IP addresses, got %q", providerPath, rawIP)
		}
		approved := false
		for _, network := range approvedNetworks {
			if network.Contains(ip) {
				approved = true
				break
			}
		}
		if !approved {
			return fmt.Errorf("config: %s.endpoint_host_ips entry %q must be contained in approved system_egress_cidrs", providerPath, rawIP)
		}
	}

	switch provider.SystemEgressMode {
	case "cilium-fqdn":
		if len(provider.SystemEgressFQDNs) == 0 {
			return fmt.Errorf("config: %s.system_egress_fqdns must not be empty in cilium-fqdn mode", providerPath)
		}
	}
	for _, fqdn := range provider.SystemEgressFQDNs {
		if strings.TrimSpace(fqdn) == "" {
			return fmt.Errorf("config: %s.system_egress_fqdns must not contain empty names", providerPath)
		}
		if strings.Contains(fqdn, "*") {
			return fmt.Errorf("config: %s.system_egress_fqdns must not contain wildcard names, got %q", providerPath, fqdn)
		}
	}

	if !isCanonicalRelativePrefix(filesystem.SubPath) {
		return fmt.Errorf("config: storage.filesystem.sub_path must be a canonical relative prefix, got %q", filesystem.SubPath)
	}
	if err := mounter.CheckProductionProfile(filesystem.Provider, provider.Profile); err != nil {
		return fmt.Errorf("config: %s.profile %q is not production-ready: %w", providerPath, provider.Profile, err)
	}

	return nil
}

func hasTemporaryCredentialFields(filesystem FileSystemConfig) bool {
	files := filesystem.CredentialFiles
	return filesystem.SessionToken != "" || filesystem.CredentialExpiry != "" ||
		files.SessionTokenFile != "" || files.CredentialExpiryFile != ""
}

func positiveQuantity(path, value string) (resource.Quantity, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil || quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("config: %s must be a positive Kubernetes quantity, got %q", path, value)
	}
	return quantity, nil
}

func isDigestPinnedImage(image string) bool {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	digested, ok := named.(reference.Digested)
	if !ok {
		return false
	}
	digest := digested.Digest()
	encoded := digest.Encoded()
	if digest.Algorithm() != "sha256" || len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	for _, r := range encoded {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

var fuseProviderEnvKeys = []string{
	"driver",
	"profile",
	"storage_identity",
	"mounter_image",
	"docker_image",
	"ca_secret_key",
	"credential_generation",
	"endpoint_host_ips",
	"lsm_profile",
	"system_egress_mode",
	"dns_cidrs",
	"system_egress_fqdns",
	"system_egress_cidrs",
	"endpoint_ports",
	"proxy_url",
}

func bindFUSEProviderEnv(v *viper.Viper) error {
	for _, provider := range []string{"minio", "obs"} {
		for _, key := range fuseProviderEnvKeys {
			configKey := "workspace.providers." + provider + "." + key
			if err := v.BindEnv(configKey); err != nil {
				return fmt.Errorf("config: bind env for %q: %w", configKey, err)
			}
		}
	}
	return nil
}

func isCanonicalRelativePrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	if !utf8.ValidString(prefix) || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return false
	}
	segments := strings.Split(prefix, "/")
	if segments[0] == ".sandbox-system" {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			if unicode.IsControl(r) {
				return false
			}
		}
	}

	return true
}

// setDefaults registers all default values on the viper instance.
func setDefaults(v *viper.Viper) {
	// Server
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.host", "0.0.0.0")

	// Runtime
	v.SetDefault("runtime.type", "docker")
	v.SetDefault("runtime.docker.host", "")
	v.SetDefault("runtime.docker.workspace_secret_root", "/var/lib/sandbox/workspace-secrets")
	v.SetDefault("runtime.kubernetes.kubeconfig", "")
	v.SetDefault("runtime.kubernetes.namespace", "")

	// Pool
	v.SetDefault("pool.min_size", 3)
	v.SetDefault("pool.max_size", 20)
	v.SetDefault("pool.refill_interval_seconds", 10)

	// Images
	v.SetDefault("images.sandbox", "sandbox:latest")
	v.SetDefault("images.gateway", "sandbox-gateway:latest")

	// Storage — Redis
	v.SetDefault("storage.state.redis.addr", "localhost:6379")
	v.SetDefault("storage.state.redis.password", "")
	v.SetDefault("storage.state.redis.db", 0)

	// Storage — FileSystem
	v.SetDefault("storage.filesystem.provider", "local")
	v.SetDefault("storage.filesystem.bucket", "")
	v.SetDefault("storage.filesystem.region", "")
	v.SetDefault("storage.filesystem.endpoint", "")
	v.SetDefault("storage.filesystem.access_key", "")
	v.SetDefault("storage.filesystem.secret_key", "")
	v.SetDefault("storage.filesystem.session_token", "")
	v.SetDefault("storage.filesystem.credential_expiry", "")
	v.SetDefault("storage.filesystem.credential_files.access_key_file", "")
	v.SetDefault("storage.filesystem.credential_files.secret_key_file", "")
	v.SetDefault("storage.filesystem.credential_files.session_token_file", "")
	v.SetDefault("storage.filesystem.credential_files.credential_expiry_file", "")
	v.SetDefault("storage.filesystem.ca_file", "")
	v.SetDefault("storage.filesystem.local_path", "/tmp/sandbox-storage")
	v.SetDefault("storage.filesystem.sub_path", "")
	v.SetDefault("storage.filesystem.use_ssl", false)

	// Workspace
	v.SetDefault("workspace.auto_sync_interval_seconds", 0)
	v.SetDefault("workspace.mode", "sync")
	v.SetDefault("workspace.secret_name", "")
	v.SetDefault("workspace.cache_size", "2Gi")
	v.SetDefault("workspace.cache_medium", "disk")
	v.SetDefault("workspace.mount_timeout_seconds", 30)
	v.SetDefault("workspace.flush_timeout_seconds", 30)
	v.SetDefault("workspace.unmount_timeout_seconds", 15)
	v.SetDefault("workspace.recreate_max_attempts", 1)
	v.SetDefault("workspace.lease_ttl_seconds", 120)
	v.SetDefault("workspace.lease_renew_interval_seconds", 30)
	v.SetDefault("workspace.quota_mode", "soft")
	v.SetDefault("workspace.mounter_resources.cpu_request", "50m")
	v.SetDefault("workspace.mounter_resources.cpu_limit", "1")
	v.SetDefault("workspace.mounter_resources.memory_request", "64Mi")
	v.SetDefault("workspace.mounter_resources.memory_limit", "512Mi")
	v.SetDefault("workspace.mounter_resources.ephemeral_storage_request", "512Mi")
	v.SetDefault("workspace.mounter_resources.ephemeral_storage_limit", "3Gi")
	v.SetDefault("workspace.fuse_pool.min_size", 3)
	v.SetDefault("workspace.fuse_pool.max_size", 20)
	v.SetDefault("workspace.fuse_pool.refill_interval_seconds", 10)
	v.SetDefault("workspace.fuse_pool.prepare_timeout_seconds", 120)

	// Security
	v.SetDefault("security.api_key", "")
	v.SetDefault("security.rate_limit", 0)
	v.SetDefault("security.exec_timeout_seconds", 30)
	v.SetDefault("security.max_exec_timeout_seconds", 600)
	v.SetDefault("security.sandbox_timeout_seconds", 3600)
	v.SetDefault("security.max_memory", "256Mi")
	v.SetDefault("security.max_disk", "100Mi")
	v.SetDefault("security.max_tmp_disk", "50Mi")
	v.SetDefault("security.max_pids", 100)
	v.SetDefault("security.max_upload_bytes", int64(2<<30))
	v.SetDefault("security.network_enabled", false)
	v.SetDefault("security.network_whitelist", []string{})
	v.SetDefault("security.seccomp_profile", "")

	// Telemetry
	v.SetDefault("telemetry.service_name", "sandbox")
	v.SetDefault("telemetry.service_version", "v1.0.0")
	v.SetDefault("telemetry.tracer.otlp_enabled", "false")
	v.SetDefault("telemetry.tracer.otlp_endpoint", "http://127.0.0.1:4318")
	v.SetDefault("telemetry.metrics.otlp_enabled", "false")
	v.SetDefault("telemetry.metrics.otlp_endpoint", "http://127.0.0.1:4318")
	v.SetDefault("telemetry.log.otlp_enabled", "false")
	v.SetDefault("telemetry.log.otlp_endpoint", "127.0.0.1:4318")
}
