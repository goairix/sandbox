package docker

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	dnetwork "github.com/docker/docker/api/types/network"

	"github.com/goairix/sandbox/internal/runtime"
)

const (
	dockerWorkspaceSecretRoot = "/var/lib/sandbox/workspace-secrets"
	dockerMounterSecretPath   = "/run/secrets/workspace"
	dockerMounterCachePath    = "/var/cache/s3fs"
	dockerMounterRunPath      = "/run/s3fs"
	dockerWorkspacePath       = "/workspace"
	dockerRunTmpfsSize        = 16 * 1024 * 1024
	dockerWorkspaceTmpfsSize  = 64 * 1024
)

var digestPinnedImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

// imageForSpec returns the Docker image to use for a sandbox spec.
func imageForSpec(spec runtime.SandboxSpec) string {
	if spec.Image != "" {
		return spec.Image
	}
	// Default images based on labels
	lang, ok := spec.Labels["language"]
	if !ok {
		return "sandbox-bash:latest"
	}
	switch lang {
	case "python":
		return "sandbox-python:latest"
	case "nodejs":
		return "sandbox-nodejs:latest"
	default:
		return "sandbox-bash:latest"
	}
}

// createContainerConfig builds Docker container configuration from a SandboxSpec.
func createContainerConfig(spec runtime.SandboxSpec) (*container.Config, *container.HostConfig, error) {
	return createContainerConfigWithSecretRoot(spec, dockerWorkspaceSecretRoot)
}

func createContainerConfigWithSecretRoot(spec runtime.SandboxSpec, secretRoot string) (*container.Config, *container.HostConfig, error) {
	if spec.WorkspaceFUSE != nil {
		return createFUSEContainerConfig(spec, secretRoot)
	}
	config := &container.Config{
		Image:      imageForSpec(spec),
		Labels:     spec.Labels,
		WorkingDir: "/workspace",
		Tty:        false,
		// Keep container running with a sleep process
		Cmd: []string{"sleep", "infinity"},
	}

	// Parse memory limit
	var memoryBytes int64
	if spec.Memory != "" {
		var err error
		memoryBytes, err = parseMemory(spec.Memory)
		if err != nil {
			return nil, nil, fmt.Errorf("parse memory: %w", err)
		}
	}

	tmpDisk := spec.TmpDisk
	if tmpDisk == "" {
		tmpDisk = runtime.DefaultTmpDisk
	}
	tmpDiskBytes, err := parseMemory(tmpDisk)
	if err != nil {
		return nil, nil, fmt.Errorf("parse tmp disk: %w", err)
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:    memoryBytes,
			PidsLimit: int64Ptr(int64(spec.PidLimit)),
		},
		ReadonlyRootfs: spec.ReadOnlyRootFS,
		SecurityOpt:    []string{},
		// Tmpfs for writable directories on read-only root
		Tmpfs: map[string]string{
			"/tmp": fmt.Sprintf("size=%d", tmpDiskBytes),
		},
	}

	// Apply bind mounts
	for _, m := range spec.Mounts {
		opt := "rw"
		if m.ReadOnly {
			opt = "ro"
		}
		hostConfig.Binds = append(hostConfig.Binds, fmt.Sprintf("%s:%s:%s", m.HostPath, m.ContainerPath, opt))
	}

	if spec.SeccompProfile != "" {
		hostConfig.SecurityOpt = append(hostConfig.SecurityOpt,
			fmt.Sprintf("seccomp=%s", spec.SeccompProfile))
	}

	// Drop all capabilities, add only needed ones
	hostConfig.CapDrop = []string{"ALL"}
	// NET_ADMIN is needed for dynamic network setup (ip route replace).
	// It is safe because the sandbox process runs as UID 1000 which cannot
	// use NET_ADMIN (non-root processes don't retain effective capabilities).
	hostConfig.CapAdd = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "NET_ADMIN"}

	// Run as non-root user
	if spec.RunAsUser > 0 {
		config.User = fmt.Sprintf("%d", spec.RunAsUser)
	}

	return config, hostConfig, nil
}

func createFUSEContainerConfig(spec runtime.SandboxSpec, secretRoot string) (*container.Config, *container.HostConfig, error) {
	fuse := spec.WorkspaceFUSE
	if fuse == nil || fuse.RuntimeType != "docker" || fuse.Driver != "s3fs" {
		return nil, nil, fmt.Errorf("valid Docker workspace FUSE spec is required")
	}
	if !digestPinnedImagePattern.MatchString(fuse.DockerImage) {
		return nil, nil, fmt.Errorf("workspace FUSE Docker image must be pinned by sha256 digest")
	}
	if spec.ID == "" || filepath.Base(spec.ID) != spec.ID || strings.ContainsAny(spec.ID, `/\\`) {
		return nil, nil, fmt.Errorf("workspace FUSE sandbox ID is invalid")
	}
	if !validDockerWorkspaceSecretRoot(secretRoot) {
		return nil, nil, fmt.Errorf("workspace FUSE secret root is invalid")
	}
	securityOpt, err := fuseSecurityOptions(fuse.LSMProfile)
	if err != nil {
		return nil, nil, err
	}
	memoryBytes, err := parseOptionalMemory(spec.Memory)
	if err != nil {
		return nil, nil, fmt.Errorf("parse memory: %w", err)
	}
	tmpDisk := spec.TmpDisk
	if tmpDisk == "" {
		tmpDisk = runtime.DefaultTmpDisk
	}
	tmpDiskBytes, err := parseMemory(tmpDisk)
	if err != nil {
		return nil, nil, fmt.Errorf("parse tmp disk: %w", err)
	}
	cacheBytes, err := parseMemory(fuse.CacheSize)
	if err != nil || fuse.CacheMedium != "disk" {
		return nil, nil, fmt.Errorf("workspace FUSE cache configuration is invalid")
	}
	labels := cloneLabels(spec.Labels)
	dns, extraHosts, err := dockerFUSEHostResolution(fuse)
	if err != nil {
		return nil, nil, err
	}
	labels["sandbox.managed"] = "true"
	labels["sandbox.id"] = spec.ID
	labels["sandbox.role"] = "fuse-runtime"
	labels["sandbox.pool.key"] = fuse.PoolKey
	labels["sandbox.workspace.cache.bytes"] = fmt.Sprintf("%d", cacheBytes)
	config := &container.Config{
		Image: fuse.DockerImage, Labels: labels, WorkingDir: "/",
		Tty: false,
	}
	host := &container.HostConfig{
		Resources: container.Resources{
			Memory: memoryBytes, PidsLimit: int64Ptr(int64(spec.PidLimit)),
			Devices: []container.DeviceMapping{{PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse", CgroupPermissions: "rwm"}},
		},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		// NET_ADMIN is required only by the trusted fixed route control. Public
		// execs run as 1000:1000 and cannot retain effective capabilities.
		CapAdd:      []string{"SYS_ADMIN", "NET_ADMIN"},
		SecurityOpt: securityOpt,
		Binds:       []string{filepath.Join(secretRoot, spec.ID) + ":" + dockerMounterSecretPath + ":ro"},
		Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: fuseCacheVolumeName(spec.ID), Target: dockerMounterCachePath}},
		Tmpfs: map[string]string{
			dockerMounterRunPath: fmt.Sprintf("size=%d,mode=0700", dockerRunTmpfsSize),
			dockerWorkspacePath:  fmt.Sprintf("size=%d,mode=0555", dockerWorkspaceTmpfsSize),
			"/tmp":               fmt.Sprintf("size=%d", tmpDiskBytes),
		},
		DNS: dns, ExtraHosts: extraHosts,
	}
	return config, host, nil
}

func validDockerWorkspaceSecretRoot(root string) bool {
	return root != "/" && filepath.IsAbs(root) && filepath.Clean(root) == root
}

func dockerFUSEHostResolution(fuse *runtime.WorkspaceFUSESpec) ([]string, []string, error) {
	dnsPorts, err := canonicalDockerPorts(fuse.SystemEgress.DNSPorts)
	if err != nil || len(dnsPorts) != 1 || dnsPorts[0] != 53 {
		return nil, nil, fmt.Errorf("Docker workspace FUSE DNS ports must be exactly 53")
	}
	dnsCIDRs, err := canonicalDockerDNSCIDRs(fuse.SystemEgress.DNSCIDRs)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace FUSE DNS CIDR is invalid")
	}
	dns := make([]string, 0, len(dnsCIDRs))
	for _, cidr := range dnsCIDRs {
		prefix, _ := netip.ParsePrefix(cidr)
		dns = append(dns, prefix.Addr().String())
	}
	_, endpointHostname, _, err := canonicalDockerFUSEEndpoint(fuse)
	if err != nil {
		return nil, nil, err
	}
	approvedCIDRs, err := canonicalDockerIPv4CIDRs(fuse.SystemEgress.EndpointCIDRs, true)
	if err != nil || len(approvedCIDRs) == 0 {
		return nil, nil, fmt.Errorf("workspace FUSE endpoint CIDR is invalid")
	}
	approved := make([]netip.Prefix, 0, len(approvedCIDRs))
	for _, cidr := range approvedCIDRs {
		prefix, _ := netip.ParsePrefix(cidr)
		approved = append(approved, prefix)
	}
	if endpointAddress, parseErr := netip.ParseAddr(endpointHostname); parseErr == nil {
		allowed := false
		for _, network := range approved {
			if network.Contains(endpointAddress) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, fmt.Errorf("workspace FUSE endpoint is outside approved system egress")
		}
	}
	var extraHosts []string
	for _, raw := range fuse.EndpointHostIPs {
		ip, parseErr := netip.ParseAddr(raw)
		if parseErr != nil || !ip.Is4() || ip.String() != raw {
			return nil, nil, fmt.Errorf("workspace FUSE endpoint host IP is invalid")
		}
		allowed := false
		for _, network := range approved {
			if network.Contains(ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, fmt.Errorf("workspace FUSE endpoint host IP is outside approved system egress")
		}
		extraHosts = append(extraHosts, endpointHostname+":"+raw)
	}
	// Docker's resolver and extra_hosts are ordered inputs. Canonical ordering
	// makes the prepared-container contract stable across equivalent config.
	extraHosts = canonicalDockerStrings(extraHosts)
	return dns, extraHosts, nil
}

// canonicalDockerFUSEEndpoint converts the provider-specific endpoint into the
// exact HTTPS URL accepted by the trusted mounter. MinIO configuration remains
// host[:port]; OBS configuration remains an explicit HTTPS URL.
func canonicalDockerFUSEEndpoint(fuse *runtime.WorkspaceFUSESpec) (string, string, int32, error) {
	const invalid = "workspace FUSE endpoint is invalid"
	if fuse == nil || !fuse.UseSSL {
		return "", "", 0, fmt.Errorf("%s", invalid)
	}
	raw := fuse.Endpoint
	var parsed *url.URL
	var err error
	switch fuse.Provider {
	case "minio":
		if strings.Contains(raw, "://") {
			return "", "", 0, fmt.Errorf("%s", invalid)
		}
		parsed, err = url.Parse("https://" + raw)
	case "obs":
		parsed, err = url.Parse(raw)
		if err == nil && parsed.Scheme != "https" {
			err = fmt.Errorf("OBS endpoint must use HTTPS")
		}
	default:
		return "", "", 0, fmt.Errorf("workspace FUSE provider is invalid")
	}
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || strings.HasSuffix(parsed.Host, ":") || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", 0, fmt.Errorf("%s", invalid)
	}
	hostname := parsed.Hostname()
	if !canonicalDockerEndpointHostname(hostname) {
		return "", "", 0, fmt.Errorf("%s", invalid)
	}
	port := int32(443)
	if rawPort := parsed.Port(); rawPort != "" {
		value, parseErr := strconv.Atoi(rawPort)
		if parseErr != nil || value < 1 || value > 65535 || strconv.Itoa(value) != rawPort {
			return "", "", 0, fmt.Errorf("%s", invalid)
		}
		port = int32(value)
	}
	ports, portErr := canonicalDockerPorts(fuse.SystemEgress.EndpointPorts)
	if portErr != nil || !containsDockerPort(ports, port) {
		return "", "", 0, fmt.Errorf("workspace FUSE endpoint port is not approved by system egress")
	}
	return parsed.String(), hostname, port, nil
}

func canonicalDockerEndpointHostname(hostname string) bool {
	if hostname == "" || hostname != strings.ToLower(hostname) || strings.HasSuffix(hostname, ".") || len(hostname) > 253 {
		return false
	}
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Is4() && address.String() == hostname
	}
	for _, label := range strings.Split(hostname, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func parseOptionalMemory(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	return parseMemory(value)
}

func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels)+5)
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func canonicalDockerStrings(values []string) []string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func fuseCacheVolumeName(id string) string {
	return "sandbox-fuse-cache-" + dockerFUSEResourceSuffix(id)
}

func fuseSecurityOptions(profile string) ([]string, error) {
	if profile == "" || strings.TrimSpace(profile) != profile || len(profile) > 128 || strings.ContainsAny(profile, ",\x00\r\n") {
		return nil, fmt.Errorf("workspace FUSE requires a confined LSM profile")
	}
	lower := strings.ToLower(profile)
	if lower == "unconfined" || lower == "apparmor=unconfined" || lower == "label=disable" {
		return nil, fmt.Errorf("workspace FUSE requires a confined LSM profile")
	}
	if strings.HasPrefix(profile, "apparmor=") {
		name := strings.TrimPrefix(profile, "apparmor=")
		if !validDockerLSMName(name, true) {
			return nil, fmt.Errorf("workspace FUSE LSM profile is invalid")
		}
		return []string{"no-new-privileges=true", "apparmor=" + name}, nil
	}
	if strings.HasPrefix(profile, "label=") {
		name := strings.TrimPrefix(profile, "label=type:")
		if name == profile || !validDockerLSMName(name, false) || name == "spc_t" || name == "unconfined_t" {
			return nil, fmt.Errorf("workspace FUSE LSM profile is invalid")
		}
		return []string{"no-new-privileges=true", "label=type:" + name}, nil
	}
	if !validDockerLSMName(profile, true) {
		return nil, fmt.Errorf("workspace FUSE LSM profile is invalid")
	}
	return []string{"no-new-privileges=true", "apparmor=" + profile}, nil
}

func validDockerLSMName(name string, allowSlash bool) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.EqualFold(name, "unconfined") {
		return false
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char) || (allowSlash && char == '/') {
			continue
		}
		return false
	}
	return true
}

// parseMemory converts "256Mi" to bytes. Returns an error if the input is invalid.
func parseMemory(s string) (int64, error) {
	var value int64
	var unit string
	n, _ := fmt.Sscanf(s, "%d%s", &value, &unit)
	if n == 0 || value <= 0 {
		return 0, fmt.Errorf("invalid memory format: %q", s)
	}
	switch unit {
	case "Ki":
		return value * 1024, nil
	case "Mi":
		return value * 1024 * 1024, nil
	case "Gi":
		return value * 1024 * 1024 * 1024, nil
	case "":
		return value, nil
	default:
		return 0, fmt.Errorf("unknown memory unit: %q in %q", unit, s)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

// createContainer creates a Docker container from spec.
func createContainer(ctx context.Context, cli dockerAPI, spec runtime.SandboxSpec, networkID, secretRoot string) (string, error) {
	config, hostConfig, err := createContainerConfigWithSecretRoot(spec, secretRoot)
	if err != nil {
		return "", err
	}

	// Specify the target network at creation time to avoid connecting to the
	// default bridge network. This is critical for network isolation.
	var networkingConfig *dnetwork.NetworkingConfig
	if networkID != "" {
		// We need the network name for EndpointsConfig key.
		// Inspect the network to get its name.
		netInspect, inspectErr := cli.NetworkInspect(ctx, networkID, dnetwork.InspectOptions{})
		if inspectErr != nil {
			return "", fmt.Errorf("inspect network: %w", inspectErr)
		}
		networkingConfig = &dnetwork.NetworkingConfig{
			EndpointsConfig: map[string]*dnetwork.EndpointSettings{
				netInspect.Name: {},
			},
		}
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, spec.ID)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	return resp.ID, nil
}
