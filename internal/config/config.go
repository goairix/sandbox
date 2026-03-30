package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config is the root configuration structure for the sandbox service.
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Runtime  RuntimeConfig  `mapstructure:"runtime"`
	Pool     PoolConfig     `mapstructure:"pool"`
	Storage  StorageConfig  `mapstructure:"storage"`
	Security SecurityConfig `mapstructure:"security"`
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
	Host string `mapstructure:"host"`
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

// StorageConfig holds storage backend settings.
type StorageConfig struct {
	State  StateStorageConfig  `mapstructure:"state"`
	Object ObjectStorageConfig `mapstructure:"object"`
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

// ObjectStorageConfig holds object storage settings.
// Provider can be one of: s3, cos, obs, oss, local.
type ObjectStorageConfig struct {
	Provider  string `mapstructure:"provider"`
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	Endpoint  string `mapstructure:"endpoint"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	LocalPath string `mapstructure:"local_path"`
}

// SecurityConfig holds sandbox security constraints.
type SecurityConfig struct {
	ExecTimeoutSeconds int      `mapstructure:"exec_timeout_seconds"`
	MaxMemory          string   `mapstructure:"max_memory"`
	MaxDisk            string   `mapstructure:"max_disk"`
	MaxPids            int      `mapstructure:"max_pids"`
	NetworkEnabled     bool     `mapstructure:"network_enabled"`
	NetworkWhitelist   []string `mapstructure:"network_whitelist"`
	SeccompProfile     string   `mapstructure:"seccomp_profile"`
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

	return cfg, nil
}

// setDefaults registers all default values on the viper instance.
func setDefaults(v *viper.Viper) {
	// Server
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.host", "0.0.0.0")

	// Runtime
	v.SetDefault("runtime.type", "docker")
	v.SetDefault("runtime.docker.host", "")
	v.SetDefault("runtime.kubernetes.kubeconfig", "")
	v.SetDefault("runtime.kubernetes.namespace", "")

	// Pool
	v.SetDefault("pool.min_size", 3)
	v.SetDefault("pool.max_size", 20)
	v.SetDefault("pool.refill_interval_seconds", 10)

	// Storage — Redis
	v.SetDefault("storage.state.redis.addr", "localhost:6379")
	v.SetDefault("storage.state.redis.password", "")
	v.SetDefault("storage.state.redis.db", 0)

	// Storage — Object
	v.SetDefault("storage.object.provider", "local")
	v.SetDefault("storage.object.bucket", "")
	v.SetDefault("storage.object.region", "")
	v.SetDefault("storage.object.endpoint", "")
	v.SetDefault("storage.object.access_key", "")
	v.SetDefault("storage.object.secret_key", "")
	v.SetDefault("storage.object.local_path", "/tmp/sandbox-storage")

	// Security
	v.SetDefault("security.exec_timeout_seconds", 30)
	v.SetDefault("security.max_memory", "256Mi")
	v.SetDefault("security.max_disk", "100Mi")
	v.SetDefault("security.max_pids", 100)
	v.SetDefault("security.network_enabled", false)
	v.SetDefault("security.network_whitelist", []string{})
	v.SetDefault("security.seccomp_profile", "")
}

