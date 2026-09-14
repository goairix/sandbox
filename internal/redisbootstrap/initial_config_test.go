package redisbootstrap

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func initialConfigFixture(t *testing.T) (BootstrapRegistration, [3]ed25519.PublicKey, InitialConfigOptions) {
	t.Helper()
	secret, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	r := BootstrapRegistration{Cluster: ClusterState{ClusterID: "isolated", Phase: Pending, Members: [3]string{"redis-sentinel-0.redis-headless.isolated.svc.cluster.local", "redis-sentinel-1.redis-headless.isolated.svc.cluster.local", "redis-sentinel-2.redis-headless.isolated.svc.cluster.local"}}, KeyDigest: digest}
	for i := range 3 {
		r.MarkerIDs[i] = fmt.Sprintf("%032x", i+1)
	}
	o := InitialConfigOptions{MasterName: "sandbox", DataPassword: strings.Repeat("d", 40), SentinelPassword: strings.Repeat("s", 40), DownAfterMilliseconds: 10000, FailoverTimeoutMilliseconds: 60000, ParallelSyncs: 1}
	return r, keys, o
}

func TestInitialMemberConfigs(t *testing.T) {
	r, keys, o := initialConfigFixture(t)
	for i := range 3 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			identity := VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[i], Member: Member{DNS: r.Cluster.Members[i], Ordinal: i}, InitialConfig: Reserved}
			redisConfig, sentinelConfig, err := RenderInitialMemberConfigs(r, keys, identity, o)
			if err != nil {
				t.Fatalf("initial configuration unavailable: %v", err)
			}
			snapshot, err := ParsePersistentConfigs(r.Cluster, identity.Member, o.MasterName, redisConfig, sentinelConfig)
			if err != nil {
				t.Fatalf("invalid generated configuration: %v", err)
			}
			wantRole := Replica
			if i == 0 {
				wantRole = Primary
			}
			if snapshot.State.Role != wantRole || snapshot.State.PrimaryDNS != r.Cluster.Members[0] || snapshot.State.SentinelEpoch != 0 || snapshot.CurrentEpoch != 0 {
				t.Fatal("incorrect fixed initial topology")
			}
			for _, value := range []string{"appendonly yes", "appendfsync everysec", "min-replicas-to-write 1", "min-replicas-max-lag 10", "requirepass \"" + o.DataPassword + "\"", "masterauth \"" + o.DataPassword + "\"", "replica-announce-ip \"" + identity.Member.DNS + "\""} {
				if !strings.Contains(string(redisConfig), value+"\n") {
					t.Fatal("missing Redis safety or authentication setting")
				}
			}
			for _, value := range []string{"requirepass \"" + o.SentinelPassword + "\"", "sentinel sentinel-pass \"" + o.SentinelPassword + "\"", "sentinel auth-pass \"sandbox\" \"" + o.DataPassword + "\"", "sentinel resolve-hostnames yes", "sentinel announce-hostnames yes"} {
				if !strings.Contains(string(sentinelConfig), value+"\n") {
					t.Fatal("missing Sentinel authentication or DNS setting")
				}
			}
			if strings.Count(string(sentinelConfig), "sentinel known-sentinel ") != 2 || strings.Count(string(sentinelConfig), "sentinel known-replica ") != 2 {
				t.Fatal("missing fixed peers")
			}
			for j, key := range keys {
				sum := sha256.Sum256(key)
				id := hex.EncodeToString(sum[:20])
				if j == i {
					if snapshot.SentinelID != id {
						t.Fatal("incorrect local retained Sentinel identity")
					}
				} else if !strings.Contains(string(sentinelConfig), "\""+r.Cluster.Members[j]+"\" 26379 "+id+"\n") {
					t.Fatal("incorrect peer Sentinel identity binding")
				}
			}
			if strings.Contains(string(redisConfig), "NO ONE") || strings.Contains(string(sentinelConfig), "NO ONE") {
				t.Fatal("generator must not promote via commands")
			}
		})
	}
}

func TestInitialMemberConfigsRejectUnsafeInputs(t *testing.T) {
	type input struct {
		registration BootstrapRegistration
		keys         [3]ed25519.PublicKey
		identity     VolumeIdentity
		options      InitialConfigOptions
	}
	r, keys, options := initialConfigFixture(t)
	base := input{r, keys, VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[0], Member: Member{DNS: r.Cluster.Members[0], Ordinal: 0}, InitialConfig: Reserved}, options}
	cases := map[string]func(*input){
		"initialized":            func(v *input) { v.registration.Cluster.Phase = Initialized },
		"invalid phase":          func(v *input) { v.registration.Cluster.Phase = "invalid" },
		"configured":             func(v *input) { v.identity.InitialConfig = Configured },
		"wrong marker":           func(v *input) { v.identity.MarkerID = v.registration.MarkerIDs[1] },
		"invalid marker":         func(v *input) { v.identity.MarkerID = "invalid" },
		"invalid ordinal":        func(v *input) { v.identity.Member.Ordinal = 3 },
		"wrong member":           func(v *input) { v.identity.Member.DNS = v.registration.Cluster.Members[1] },
		"wrong cluster":          func(v *input) { v.identity.ClusterID = "another" },
		"wrong digest":           func(v *input) { v.registration.KeyDigest = strings.Repeat("0", 64) },
		"missing key":            func(v *input) { v.keys[1] = nil },
		"duplicate key":          func(v *input) { v.keys[1] = v.keys[0] },
		"empty master":           func(v *input) { v.options.MasterName = "" },
		"long master":            func(v *input) { v.options.MasterName = strings.Repeat("a", 129) },
		"newline master":         func(v *input) { v.options.MasterName = "sandbox\ninclude evil" },
		"space master":           func(v *input) { v.options.MasterName = "sandbox name" },
		"unicode master":         func(v *input) { v.options.MasterName = "沙盒" },
		"short data password":    func(v *input) { v.options.DataPassword = strings.Repeat("d", 31) },
		"same passwords":         func(v *input) { v.options.SentinelPassword = v.options.DataPassword },
		"long sentinel password": func(v *input) { v.options.SentinelPassword = strings.Repeat("s", 257) },
		"quote password":         func(v *input) { v.options.DataPassword = strings.Repeat("d", 32) + "\"" },
		"space password":         func(v *input) { v.options.SentinelPassword = strings.Repeat("s", 32) + " " },
		"unicode password":       func(v *input) { v.options.DataPassword = strings.Repeat("d", 32) + "沙" },
		"small down after":       func(v *input) { v.options.DownAfterMilliseconds = 999 },
		"large down after":       func(v *input) { v.options.DownAfterMilliseconds = 60001 },
		"short failover":         func(v *input) { v.options.FailoverTimeoutMilliseconds = 19999 },
		"large failover":         func(v *input) { v.options.FailoverTimeoutMilliseconds = 600001 },
		"zero syncs":             func(v *input) { v.options.ParallelSyncs = 0 },
		"three syncs":            func(v *input) { v.options.ParallelSyncs = 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v := base
			mutate(&v)
			redisConfig, sentinelConfig, err := RenderInitialMemberConfigs(v.registration, v.keys, v.identity, v.options)
			if err == nil || redisConfig != nil || sentinelConfig != nil {
				t.Fatal("unsafe initial configuration accepted or leaked")
			}
			if strings.Contains(err.Error(), v.options.DataPassword) || strings.Contains(err.Error(), v.options.SentinelPassword) {
				t.Fatal("configuration error exposed credentials")
			}
		})
	}
}

func TestInitialMemberConfigsBoundariesAndDeterminism(t *testing.T) {
	r, keys, options := initialConfigFixture(t)
	identity := VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[2], Member: Member{DNS: r.Cluster.Members[2], Ordinal: 2}, InitialConfig: Reserved}
	for _, budget := range []InitialConfigOptions{
		{MasterName: "_sandbox-0", DataPassword: strings.Repeat("d", 32), SentinelPassword: strings.Repeat("s", 32), DownAfterMilliseconds: 1000, FailoverTimeoutMilliseconds: 2000, ParallelSyncs: 1},
		{MasterName: strings.Repeat("a", 128), DataPassword: strings.Repeat("d", 256), SentinelPassword: strings.Repeat("s", 256), DownAfterMilliseconds: 60000, FailoverTimeoutMilliseconds: 600000, ParallelSyncs: 2},
	} {
		redisConfig, sentinelConfig, err := RenderInitialMemberConfigs(r, keys, identity, budget)
		if err != nil || len(redisConfig) == 0 || len(sentinelConfig) == 0 {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
	oneR, oneS, err := RenderInitialMemberConfigs(r, keys, identity, options)
	if err != nil {
		t.Fatal(err)
	}
	twoR, twoS, err := RenderInitialMemberConfigs(r, keys, identity, options)
	if err != nil || string(oneR) != string(twoR) || string(oneS) != string(twoS) {
		t.Fatal("deterministic initial configuration changed")
	}
}
