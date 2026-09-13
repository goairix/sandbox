package redisbootstrap

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const persistentTestID = "0123456789abcdef0123456789abcdef01234567"

func persistentFixture(c ClusterState, local, primary int) (string, string) {
	r := "port 6379\nrequirepass \"safe # password\\\"\\\\\"\nappendonly yes\n"
	if local != primary {
		r += fmt.Sprintf("replicaof %s 6379\n", c.Members[primary])
	}
	s := fmt.Sprintf("port 26379\nsentinel monitor main %s 6379 2\nsentinel config-epoch main 7\nsentinel current-epoch 11\nsentinel myid %s\n", c.Members[primary], persistentTestID)
	return r, s
}

func TestPersistentRedisSDSGrammar(t *testing.T) {
	tests := []struct {
		name, line string
		want       []string
		bad        bool
	}{
		{"double escapes", `requirepass "a # b\"c\\d\x21\q"`, []string{"requirepass", `a # b"c\d!q`}, false},
		{"single quote", `requirepass 'a\'b\n#c'`, []string{"requirepass", `a'b\n#c`}, false},
		{"literal unquoted slash", `requirepass abc\def#ghi`, []string{"requirepass", `abc\def#ghi`}, false},
		{"prefix quote", `requirepass pre"fix value"`, []string{"requirepass", "prefix value"}, false},
		{"empty quoted", `save ""`, []string{"save", ""}, false},
		{"invalid hex literal", `requirepass "\xz1"`, []string{"requirepass", "xz1"}, false},
		{"closing suffix", `requirepass "a"b`, nil, true},
		{"unclosed single", `requirepass 'a`, nil, true},
		{"unclosed slash", `requirepass "a\`, nil, true},
		{"decoded newline", `requirepass "a\n"`, nil, true},
		{"decoded carriage", `requirepass "a\r"`, nil, true},
		{"decoded tab", `requirepass "a\t"`, nil, true},
		{"decoded bell", `requirepass "a\a"`, nil, true},
		{"decoded backspace", `requirepass "a\b"`, nil, true},
		{"decoded invalid UTF8", `requirepass "\xff"`, nil, true},
		{"decoded Unicode control", `requirepass "\xc2\x85"`, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := persistentSplitArgs(tc.line)
			if (err != nil) != tc.bad || (!tc.bad && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("tokens mismatch, error=%v", err)
			}
		})
	}
}

// Manually quoted values exercise valid input grammar, not Sentinel's rewrite
// escaping: Redis 7.4 emits several Sentinel auth values using raw %s.
func TestPersistentConfigsManuallyQuotedValuesAndExtraSettings(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 0, 2)
	r += "# Extra Redis settings with manually quoted values\nsave \"\"\nlatency-tracking-info-percentiles 50 99 99.9\nreplica-announce-ip \"" + c.Members[0] + "\"\nreplica-announce-port 6379\n"
	s += "# Extra Sentinel settings with manually quoted values\nsentinel leader-epoch main 12\nsentinel known-replica main " + c.Members[0] + " 6379\nsentinel known-slave main " + c.Members[1] + " 6379\nsentinel known-sentinel main " + c.Members[1] + " 26379 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsentinel resolve-hostnames yes\nsentinel announce-hostnames yes\nsentinel announce-ip \"" + c.Members[0] + "\"\nsentinel announce-port 26379\nsentinel auth-pass main \"s # p\\\"\\\\\"\nsentinel auth-user main default\nsentinel sentinel-user default\nsentinel sentinel-pass 's # p'\nsentinel deny-scripts-reconfig yes\nsentinel down-after-milliseconds main 5000\nsentinel failover-timeout main 60000\nsentinel parallel-syncs main 1\nsentinel master-reboot-down-after-period main 0\n"
	got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r), []byte(s))
	if err != nil || got.State.SentinelEpoch != 7 || got.CurrentEpoch != 11 {
		t.Fatalf("quoted configuration snapshot = %+v, %v", got, err)
	}
}

func TestPersistentConfigsAcceptedEpochBoundaries(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 2, 2)
	for _, tc := range []struct {
		name, config, current   string
		wantConfig, wantCurrent uint64
	}{
		{"initial zero epochs", "0", "0", 0, 0},
		{"maximum uint64 epochs", "18446744073709551615", "18446744073709551615", 18446744073709551615, 18446744073709551615},
		{"maximum epoch separation", "0", "18446744073709551615", 0, 18446744073709551615},
		{"high distinct epochs", "18446744073709551614", "18446744073709551615", 18446744073709551614, 18446744073709551615},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.ReplaceAll(s, "config-epoch main 7", "config-epoch main "+tc.config)
			input = strings.ReplaceAll(input, "current-epoch 11", "current-epoch "+tc.current)
			got, err := ParsePersistentConfigs(c, testMember(c, 2), "main", []byte(r), []byte(input))
			want := PersistentConfigSnapshot{State: PersistedState{Member: testMember(c, 2), Role: Primary, PrimaryDNS: c.Members[2], SentinelEpoch: tc.wantConfig}, SentinelID: persistentTestID, CurrentEpoch: tc.wantCurrent}
			if err != nil || got != want {
				t.Fatalf("boundary snapshot = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestPersistentConfigsDirectiveMatchingIsASCII(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 0, 2)
	for _, tc := range []struct{ name, extra string }{
		{"Kelvin sign not ASCII K", "sentinel Known-replica main " + c.Members[1] + " 6379\n"},
		{"long s not ASCII s", "sentinel resolve-hostnames yeſ\n"},
		{"long s not Sentinel keyword", "ſentinel monitor main " + c.Members[2] + " 6379 2\n"},
		{"empty keyword", "\"\" value\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r), []byte(s+tc.extra))
			if err == nil || got != (PersistentConfigSnapshot{}) {
				t.Fatal("accepted Unicode case folding Redis does not perform")
			}
		})
	}
	if _, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r+"Key value\n"), []byte(s)); err == nil {
		t.Fatal("accepted non-ASCII Redis directive")
	}
	// Redis's byte-ASCII case insensitivity does not restrict Unicode values.
	r = strings.ReplaceAll(r, "port 6379", "PoRt 6379")
	r = strings.ReplaceAll(r, "replicaof", "RePlIcAoF")
	s = strings.ReplaceAll(s, "sentinel", "SeNtInEl")
	s = strings.ReplaceAll(s, "monitor", "MoNiToR")
	s += "SeNtInEl KnOwN-RePlIcA main " + c.Members[1] + " 6379\nSeNtInEl ReSoLvE-HoStNaMeS YeS\nSeNtInEl AuTh-PaSs main \"密码 K ſ # value\"\n"
	got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r), []byte(s))
	want := PersistentConfigSnapshot{State: PersistedState{Member: testMember(c, 0), Role: Replica, PrimaryDNS: c.Members[2], SentinelEpoch: 7}, SentinelID: persistentTestID, CurrentEpoch: 11}
	if err != nil || got != want {
		t.Fatalf("mixed ASCII case with Unicode auth snapshot = %+v, %v", got, err)
	}
}

// These source-derived lines follow Redis 7.4 sentinel.c's
// rewriteConfigSentinelOption raw-%s formats, not a live CONFIG REWRITE run.
// auth-pass/auth-user and sentinel-pass are not escaped there; safe single-token
// values form valid files, while complex raw values can form invalid files.
func TestPersistentConfigsSentinel74RawRewriteFormats(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 0, 2)
	safe := s + "sentinel auth-pass main safe#token\\literal\nsentinel auth-user main default\nsentinel sentinel-pass safe#peer\\literal\n"
	got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r), []byte(safe))
	want := PersistentConfigSnapshot{State: PersistedState{Member: testMember(c, 0), Role: Replica, PrimaryDNS: c.Members[2], SentinelEpoch: 7}, SentinelID: persistentTestID, CurrentEpoch: 11}
	if err != nil || got != want {
		t.Fatalf("safe source-derived raw format snapshot = %+v, %v", got, err)
	}
	for _, directive := range []struct{ name, format string }{{"auth-pass", "sentinel auth-pass main %s\n"}, {"auth-user", "sentinel auth-user main %s\n"}, {"sentinel-pass", "sentinel sentinel-pass %s\n"}} {
		for _, credential := range []struct{ name, secret string }{{"raw whitespace", "do-not-leak-raw-secret # tail"}, {"raw unbalanced quote", "do-not-leak-raw-secret\"unterminated"}} {
			t.Run(directive.name+"/"+credential.name, func(t *testing.T) {
				broken := s + fmt.Sprintf(directive.format, credential.secret)
				got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r), []byte(broken))
				if err == nil || got != (PersistentConfigSnapshot{}) {
					t.Fatal("accepted malformed source-derived raw rewrite")
				}
				if strings.Contains(err.Error(), "do-not-leak-raw-secret") || strings.Contains(err.Error(), credential.secret) {
					t.Fatal("broken raw rewrite error leaked credential")
				}
			})
		}
	}
}

func TestPersistentConfigsAdditionalBoundaries(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 0, 2)
	for _, tc := range []struct{ name, redis, sentinel string }{
		{"invalid numeric setting", r, s + "sentinel down-after-milliseconds main garbage\n"},
		{"duplicate announce alias", r + "replica-announce-ip " + c.Members[0] + "\nslave-announce-ip " + c.Members[1] + "\n", s},
		{"foreign auth", r, s + "sentinel auth-pass other do-not-leak-secret\n"},
		{"foreign epoch", r, strings.ReplaceAll(s, "config-epoch main", "config-epoch other")},
		{"duplicate epoch", r, s + "sentinel config-epoch main 7\n"},
		{"bad ID", r, strings.ReplaceAll(s, persistentTestID, strings.ToUpper(persistentTestID))},
		{"duplicate myid", r, s + "sentinel myid " + persistentTestID + "\n"},
		{"missing monitor", r, strings.ReplaceAll(s, "sentinel monitor main "+c.Members[2]+" 6379 2\n", "")},
		{"negative epoch", r, strings.ReplaceAll(s, "current-epoch 11", "current-epoch -1")},
		{"plus epoch", r, strings.ReplaceAll(s, "current-epoch 11", "current-epoch +11")},
		{"empty epoch", r, strings.ReplaceAll(s, "current-epoch 11", `current-epoch ""`)},
		{"wrong Redis port", strings.ReplaceAll(r, "port 6379", "port 6380"), s},
		{"duplicate Redis port", r + "port 6379\n", s},
		{"wrong Sentinel port", r, strings.ReplaceAll(s, "port 26379", "port 26380")},
		{"duplicate Sentinel port", r, s + "port 26379\n"},
		{"Sentinel replica role", r, s + "replicaof " + c.Members[2] + " 6379\n"},
		{"duplicate known alias", r, s + "sentinel known-replica main " + c.Members[1] + " 6379\nsentinel known-slave main " + c.Members[1] + " 6379\n"},
		{"old known sentinel form", r, s + "sentinel known-sentinel main " + c.Members[1] + " 26379\n"},
		{"peer self", r, s + "sentinel known-sentinel main " + c.Members[0] + " 26379 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"},
		{"peer duplicate ID", r, s + "sentinel known-sentinel main " + c.Members[1] + " 26379 " + persistentTestID + "\n"},
		{"DNS disabled", r, s + "sentinel resolve-hostnames no\n"},
		{"announce old IP", r, s + "sentinel announce-ip 10.1.2.3\n"},
		{"Redis announce old IP", r + "replica-announce-ip 10.1.2.3\n", s},
		{"scripts", r, s + "sentinel notification-script main /tmp/run\n"},
		{"module", r + "loadmodule /tmp/lib.so\n", s},
		{"unknown token only", r + "unknown\n", s},
		{"raw Unicode control", r + "#\u0085\n", s},
		{"quoted raw tab", r + "masterauth \"do-not-leak-secret\t\"\n", s},
		{"oversize file", strings.Repeat("#", maximumPersistentConfigBytes+1), s},
		{"oversize line", strings.Repeat("#", maximumPersistentLineBytes+1), s},
		{"too many lines", strings.Repeat("\n", maximumPersistentLines), s},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(tc.redis), []byte(tc.sentinel))
			if err == nil || got != (PersistentConfigSnapshot{}) {
				t.Fatalf("accepted invalid configuration: %+v", got)
			}
			if strings.Contains(err.Error(), "do-not-leak-secret") {
				t.Fatal("credential leak")
			}
		})
	}
}

func TestPersistentConfigsRolesAndEpochs(t *testing.T) {
	c := testCluster()
	for _, tc := range []struct {
		name           string
		local, primary int
		role           Role
	}{
		{"initial primary", 0, 0, Primary}, {"initial replica", 1, 0, Replica},
		{"promoted nonzero primary", 2, 2, Primary}, {"old primary replica", 0, 2, Replica},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := persistentFixture(c, tc.local, tc.primary)
			got, err := ParsePersistentConfigs(c, testMember(c, tc.local), "main", []byte(r), []byte(s))
			want := PersistentConfigSnapshot{State: PersistedState{Member: testMember(c, tc.local), Role: tc.role, PrimaryDNS: c.Members[tc.primary], SentinelEpoch: 7}, SentinelID: persistentTestID, CurrentEpoch: 11}
			if err != nil || got != want {
				t.Fatalf("snapshot = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestPersistentConfigsRejectInvalidEvidence(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 0, 2)
	tests := []struct{ name, redis, sentinel string }{
		{"missing myid", r, strings.ReplaceAll(s, "sentinel myid "+persistentTestID+"\n", "")},
		{"missing config epoch", r, strings.ReplaceAll(s, "sentinel config-epoch main 7\n", "")},
		{"missing current epoch", r, strings.ReplaceAll(s, "sentinel current-epoch 11\n", "")},
		{"lower current epoch", r, strings.ReplaceAll(s, "current-epoch 11", "current-epoch 6")},
		{"overflow epoch", r, strings.ReplaceAll(s, "config-epoch main 7", "config-epoch main 18446744073709551616")},
		{"noncanonical epoch", r, strings.ReplaceAll(s, "config-epoch main 7", "config-epoch main 07")},
		{"duplicate alias", r + "slaveof " + c.Members[2] + " 6379\n", s},
		{"self replica", strings.ReplaceAll(r, c.Members[2], c.Members[0]), s},
		{"external replica", strings.ReplaceAll(r, c.Members[2], "external.ns"), s},
		{"stale IP", r, strings.ReplaceAll(s, c.Members[2], "10.1.2.3")},
		{"role conflict", "port 6379\n", s},
		{"monitor conflict", r, strings.ReplaceAll(s, c.Members[2], c.Members[1])},
		{"duplicate monitor", r, s + "sentinel monitor main " + c.Members[2] + " 6379 2\n"},
		{"wrong quorum", r, strings.ReplaceAll(s, "6379 2", "6379 3")},
		{"foreign master", r, s + "sentinel known-replica other " + c.Members[1] + " 6379\n"},
		{"unknown same master", r, s + "sentinel unknown main harmless\n"},
		{"include Redis", r + "include private.conf\n", s},
		{"include Sentinel", r, s + "include private.conf\n"},
		{"empty Redis", "", s},
		{"missing Redis port", strings.ReplaceAll(r, "port 6379\n", ""), s},
		{"inline comment not comment", r, strings.ReplaceAll(s, "6379 2", "6379 2 # nope")},
		{"auth quote error", r + "requirepass \"do-not-leak-secret\n", s},
		{"auth decoded control", r + `masterauth "do-not-leak-secret\x00"` + "\n", s},
		{"raw NUL", r + "# do-not-leak-secret\x00\n", s},
		{"invalid UTF8", r + string([]byte{0xff}), s},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(tc.redis), []byte(tc.sentinel))
			if err == nil {
				t.Fatalf("accepted invalid evidence: %+v", got)
			}
			if got != (PersistentConfigSnapshot{}) {
				t.Fatal("error returned partial snapshot")
			}
			if strings.Contains(err.Error(), "do-not-leak-secret") {
				t.Fatal("error leaked credentials")
			}
		})
	}
}
