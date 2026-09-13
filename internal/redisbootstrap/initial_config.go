package redisbootstrap

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// InitialConfigOptions bounds the supported initial three-member configuration.
type InitialConfigOptions struct {
	MasterName                  string
	DataPassword                string
	SentinelPassword            string
	DownAfterMilliseconds       int
	FailoverTimeoutMilliseconds int
	ParallelSyncs               int
}

var errInitialConfig = errors.New("invalid initial Redis and Sentinel configuration")

// RenderInitialMemberConfigs renders initial configuration only. It does not
// verify current PVC contents or authorize replaying a Pending seed grant;
// callers must validate and durably complete the local configuration transaction.
// Configured identities and Initialized installations must never use this path.
func RenderInitialMemberConfigs(r BootstrapRegistration, keys [3]ed25519.PublicKey, identity VolumeIdentity, o InitialConfigOptions) ([]byte, []byte, error) {
	if r.Validate() != nil || r.Cluster.Phase != Pending || identity.Validate(r.Cluster) != nil || identity.InitialConfig != Reserved {
		return nil, nil, errInitialConfig
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil || digest != r.KeyDigest || identity.MarkerID != r.MarkerIDs[identity.Member.Ordinal] || !clusterIDPattern.MatchString(o.MasterName) || ValidateBuiltinSentinelPassword(o.DataPassword) != nil || ValidateBuiltinSentinelPassword(o.SentinelPassword) != nil || o.DownAfterMilliseconds < 1000 || o.DownAfterMilliseconds > 60000 || o.FailoverTimeoutMilliseconds < 2*o.DownAfterMilliseconds || o.FailoverTimeoutMilliseconds > 600000 || o.ParallelSyncs < 1 || o.ParallelSyncs > 2 {
		return nil, nil, errInitialConfig
	}
	quote := func(value string) string {
		// Fixed validated DNS and rewrite-safe values contain no controls. Keep
		// using the Redis syntax renderer rather than introducing shell quoting.
		quoted, _ := QuoteConfigValue(value)
		return quoted
	}
	var ids [3]string
	for i, key := range keys {
		sum := sha256.Sum256(key)
		ids[i] = hex.EncodeToString(sum[:20])
		for j := 0; j < i; j++ {
			if ids[i] == ids[j] {
				return nil, nil, errInitialConfig
			}
		}
	}
	redisLines := []string{
		"port 6379", "bind 0.0.0.0", "protected-mode yes", "daemonize no", "logfile \"\"", "dir \"/data\"", "appendonly yes", "appendfsync everysec", "save \"\"", "min-replicas-to-write 1", "min-replicas-max-lag 10",
		"requirepass " + quote(o.DataPassword), "masterauth " + quote(o.DataPassword), "replica-announce-ip " + quote(identity.Member.DNS), "replica-announce-port 6379",
	}
	if identity.Member.Ordinal != 0 {
		redisLines = append(redisLines, "replicaof "+quote(r.Cluster.Members[0])+" 6379")
	}
	name := quote(o.MasterName)
	sentinelLines := []string{
		"port 26379", "bind 0.0.0.0", "protected-mode yes", "daemonize no", "logfile \"\"", "dir \"/data\"", "requirepass " + quote(o.SentinelPassword),
		"sentinel resolve-hostnames yes", "sentinel announce-hostnames yes", "sentinel deny-scripts-reconfig yes", "sentinel announce-ip " + quote(identity.Member.DNS), "sentinel announce-port 26379",
		"sentinel monitor " + name + " " + quote(r.Cluster.Members[0]) + " 6379 2", "sentinel auth-pass " + name + " " + quote(o.DataPassword), "sentinel sentinel-pass " + quote(o.SentinelPassword),
		"sentinel myid " + ids[identity.Member.Ordinal], "sentinel config-epoch " + name + " 0", "sentinel leader-epoch " + name + " 0", "sentinel current-epoch 0",
		fmt.Sprintf("sentinel down-after-milliseconds %s %d", name, o.DownAfterMilliseconds), fmt.Sprintf("sentinel failover-timeout %s %d", name, o.FailoverTimeoutMilliseconds), fmt.Sprintf("sentinel parallel-syncs %s %d", name, o.ParallelSyncs),
	}
	for i, dns := range r.Cluster.Members {
		if i != 0 {
			sentinelLines = append(sentinelLines, "sentinel known-replica "+name+" "+quote(dns)+" 6379")
		}
		if i != identity.Member.Ordinal {
			sentinelLines = append(sentinelLines, "sentinel known-sentinel "+name+" "+quote(dns)+" 26379 "+ids[i])
		}
	}
	redisConfig := []byte(strings.Join(redisLines, "\n") + "\n")
	sentinelConfig := []byte(strings.Join(sentinelLines, "\n") + "\n")
	if _, err := ParsePersistentConfigs(r.Cluster, identity.Member, o.MasterName, redisConfig, sentinelConfig); err != nil {
		return nil, nil, errInitialConfig
	}
	return redisConfig, sentinelConfig, nil
}
