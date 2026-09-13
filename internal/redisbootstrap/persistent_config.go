package redisbootstrap

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumPersistentConfigBytes = 1024 * 1024
	maximumPersistentLineBytes   = 64 * 1024
	maximumPersistentLines       = 16384
)

// PersistentConfigSnapshot separates the master's mapping epoch from global
// Sentinel election state. It is not live role evidence or promotion authority.
type PersistentConfigSnapshot struct {
	State        PersistedState
	SentinelID   string
	CurrentEpoch uint64
}

// ParsePersistentConfigs interprets caller-validated private Configured PVC bytes.
// It does not validate file ownership, identity, coherent reads or remote origin.
func ParsePersistentConfigs(c ClusterState, local Member, masterName string, redisConfig, sentinelConfig []byte) (PersistentConfigSnapshot, error) {
	var out PersistentConfigSnapshot
	if err := local.Validate(c); err != nil {
		return out, err
	}
	if len(masterName) == 0 || len(masterName) > 128 || !persistentValueSafe(masterName) {
		return out, errors.New("invalid persistent master name")
	}
	r, err := persistentLines(redisConfig, "Redis")
	if err != nil {
		return out, err
	}
	s, err := persistentLines(sentinelConfig, "Sentinel")
	if err != nil {
		return out, err
	}
	if persistentDefaultACL(r) != nil || persistentDefaultACL(s) != nil {
		return out, errors.New("unsupported or inconsistent persistent default ACL")
	}
	state := PersistedState{Member: local, Role: Primary}
	seenRedis := map[string]bool{}
	for _, a := range r {
		key := persistentKeyword(a[0])
		if key == "" {
			return out, errors.New("invalid Redis configuration keyword")
		}
		if persistentExtension(key) || key == "sentinel" {
			return out, errors.New("unsupported Redis configuration extension")
		}
		if key == "slaveof" {
			key = "replicaof"
		}
		if key == "slave-announce-ip" {
			key = "replica-announce-ip"
		}
		if key == "slave-announce-port" {
			key = "replica-announce-port"
		}
		switch key {
		case "port", "replicaof", "replica-announce-ip", "replica-announce-port":
			if seenRedis[key] {
				return out, errors.New("duplicate critical Redis configuration")
			}
			seenRedis[key] = true
			if key == "replicaof" {
				if len(a) != 3 || !containsDNS(c, a[1]) || a[1] == local.DNS || a[2] != "6379" {
					return out, errors.New("invalid persisted replica target")
				}
				state.Role, state.PrimaryDNS = Replica, a[1]
			} else if len(a) != 2 || (key == "replica-announce-ip" && a[1] != local.DNS) || (key != "replica-announce-ip" && a[1] != "6379") {
				return out, errors.New("invalid critical Redis configuration")
			}
		}
	}
	if !seenRedis["port"] {
		return out, errors.New("incomplete persisted Redis configuration")
	}
	seen := map[string]bool{}
	var primary, id string
	var configEpoch, currentEpoch uint64
	known := map[string]bool{}
	knownIDs := map[string]bool{}
	for _, a := range s {
		key := persistentKeyword(a[0])
		if key == "" {
			return out, errors.New("invalid Sentinel configuration keyword")
		}
		if persistentExtension(key) {
			return out, errors.New("unsupported Sentinel configuration extension")
		}
		if key != "sentinel" {
			if key == "replicaof" || key == "slaveof" {
				return out, errors.New("invalid Sentinel role configuration")
			}
			if key == "port" {
				if seen["port"] || len(a) != 2 || a[1] != "26379" {
					return out, errors.New("invalid Sentinel port configuration")
				}
				seen["port"] = true
			}
			continue
		}
		if len(a) < 3 {
			return out, errors.New("invalid Sentinel configuration arity")
		}
		key = persistentKeyword(a[1])
		if key == "known-slave" {
			key = "known-replica"
		}
		if key != "known-replica" && key != "known-sentinel" {
			if seen[key] {
				return out, errors.New("duplicate Sentinel configuration")
			}
			seen[key] = true
		}
		switch key {
		case "monitor":
			if len(a) != 6 || a[2] != masterName || !containsDNS(c, a[3]) || a[4] != "6379" || a[5] != "2" {
				return out, errors.New("invalid persisted Sentinel monitor")
			}
			primary = a[3]
		case "config-epoch", "leader-epoch":
			if len(a) != 4 || a[2] != masterName {
				return out, errors.New("invalid persisted Sentinel master epoch")
			}
			epoch, err := persistentEpoch(a[3])
			if err != nil {
				return out, err
			}
			if key == "config-epoch" {
				configEpoch = epoch
			}
		case "current-epoch":
			if len(a) != 3 {
				return out, errors.New("invalid persisted Sentinel global epoch")
			}
			currentEpoch, err = persistentEpoch(a[2])
			if err != nil {
				return out, err
			}
		case "myid":
			if len(a) != 3 || !persistentID(a[2]) {
				return out, errors.New("invalid persisted Sentinel identity")
			}
			id = a[2]
		case "known-replica", "known-sentinel":
			want, port := 5, "6379"
			if key == "known-sentinel" {
				want, port = 6, "26379"
			}
			if len(a) != want || a[2] != masterName || !containsDNS(c, a[3]) || a[4] != port || known[key+":"+a[3]] {
				return out, errors.New("invalid persisted Sentinel topology")
			}
			known[key+":"+a[3]] = true
			if key == "known-sentinel" {
				if a[3] == local.DNS || !persistentID(a[5]) || knownIDs[a[5]] {
					return out, errors.New("invalid persisted peer Sentinel identity")
				}
				knownIDs[a[5]] = true
			}
		case "auth-pass", "auth-user", "down-after-milliseconds", "failover-timeout", "parallel-syncs", "master-reboot-down-after-period":
			if len(a) != 4 || a[2] != masterName {
				return out, errors.New("invalid persisted Sentinel master setting")
			}
			if key != "auth-pass" && key != "auth-user" {
				n, err := persistentEpoch(a[3])
				if err != nil || n > 2147483647 || (n == 0 && key != "master-reboot-down-after-period") {
					return out, errors.New("invalid persisted Sentinel numeric setting")
				}
			}
		case "announce-ip":
			if len(a) != 3 || a[2] != local.DNS {
				return out, errors.New("invalid persisted Sentinel announce address")
			}
		case "announce-port":
			if len(a) != 3 || a[2] != "26379" {
				return out, errors.New("invalid persisted Sentinel announce port")
			}
		case "resolve-hostnames", "announce-hostnames":
			if len(a) != 3 || persistentKeyword(a[2]) != "yes" {
				return out, errors.New("persisted Sentinel must retain DNS hostnames")
			}
		case "sentinel-user", "sentinel-pass":
			if len(a) != 3 {
				return out, errors.New("invalid persisted Sentinel authentication arity")
			}
		case "deny-scripts-reconfig":
			if len(a) != 3 || persistentKeyword(a[2]) != "yes" {
				return out, errors.New("unsupported persisted Sentinel script setting")
			}
		default:
			return out, errors.New("unsupported persisted Sentinel directive")
		}
	}
	if !seen["monitor"] || !seen["config-epoch"] || !seen["current-epoch"] || !seen["myid"] || currentEpoch < configEpoch || knownIDs[id] {
		return out, errors.New("incomplete or inconsistent persisted Sentinel configuration")
	}
	if (state.Role == Primary && primary != local.DNS) || (state.Role == Replica && state.PrimaryDNS != primary) {
		return out, errors.New("persisted Redis role conflicts with Sentinel monitor")
	}
	state.PrimaryDNS, state.SentinelEpoch = primary, configEpoch
	return PersistentConfigSnapshot{State: state, SentinelID: id, CurrentEpoch: currentEpoch}, nil
}

func persistentExtension(key string) bool {
	return key == "include" || key == "loadmodule" || key == "rename-command" || key == "aclfile"
}

// Redis CONFIG REWRITE and Sentinel FLUSHCONFIG always emit the default user,
// including installations configured using requirepass rather than explicit ACL.
// Accept only that exact protected generated shape, with its one password hash
// bound to the same file's requirepass. No named users, selectors, nopass or
// external ACL files become supported by allowing this rewrite compatibility.
func persistentDefaultACL(lines [][]string) error {
	var password string
	var acl []string
	for _, args := range lines {
		switch persistentKeyword(args[0]) {
		case "requirepass":
			if len(args) != 2 || args[1] == "" || password != "" {
				return errors.New("invalid persistent default password")
			}
			password = args[1]
		case "user":
			if acl != nil || len(args) != 8 || args[1] != "default" || args[2] != "on" || args[3] != "sanitize-payload" || args[5] != "~*" || args[6] != "&*" || args[7] != "+@all" || len(args[4]) != 65 || args[4][0] != '#' || !lowerHex(args[4][1:], 32) {
				return errors.New("unsupported persistent ACL")
			}
			acl = args
		}
	}
	if acl == nil {
		return nil
	}
	if password == "" {
		return errors.New("persistent ACL has no bound password")
	}
	sum := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(acl[4][1:])) != 1 {
		return errors.New("persistent ACL password mismatch")
	}
	return nil
}

// persistentKeyword matches Redis's byte-ASCII case handling, not Unicode fold.
// Empty signals a non-ASCII/empty keyword; this restriction never applies to
// authentication values or other arbitrary UTF-8 configuration values.
func persistentKeyword(s string) string {
	keyword := []byte(s)
	for i, ch := range keyword {
		if ch >= 128 {
			return ""
		}
		if ch >= 'A' && ch <= 'Z' {
			keyword[i] = ch + ('a' - 'A')
		}
	}
	return string(keyword)
}

func persistentID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, ch := range s {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func persistentEpoch(s string) (uint64, error) {
	if len(s) == 0 || (len(s) > 1 && s[0] == '0') {
		return 0, errors.New("noncanonical persisted epoch")
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, errors.New("noncanonical persisted epoch")
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, errors.New("persisted epoch exceeds uint64")
	}
	return n, nil
}

func persistentValueSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, ch := range s {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func persistentLines(data []byte, label string) ([][]string, error) {
	if len(data) == 0 || len(data) > maximumPersistentConfigBytes || !utf8.Valid(data) {
		return nil, fmt.Errorf("%s configuration size or encoding is invalid", label)
	}
	for _, ch := range string(data) {
		if unicode.IsControl(ch) && ch != '\n' && ch != '\r' && ch != '\t' {
			return nil, fmt.Errorf("%s configuration contains raw controls", label)
		}
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > maximumPersistentLines {
		return nil, fmt.Errorf("%s configuration exceeds line count limit", label)
	}
	var result [][]string
	for i, line := range lines {
		if len(line) > maximumPersistentLineBytes {
			return nil, fmt.Errorf("%s configuration line %d exceeds size limit", label, i+1)
		}
		line = strings.Trim(line, " \t\r")
		if line == "" || line[0] == '#' {
			continue
		}
		args, err := persistentSplitArgs(line)
		if err != nil || len(args) < 2 {
			return nil, fmt.Errorf("%s configuration line %d has invalid syntax", label, i+1)
		}
		result = append(result, args)
	}
	return result, nil
}

// persistentSplitArgs follows Redis 7.4 sdssplitargs, then restricts decoded
// values to UTF-8 without controls. In particular, # is never an inline comment.
func persistentSplitArgs(line string) ([]string, error) {
	var args []string
	for i := 0; i < len(line); {
		for i < len(line) && persistentSpace(line[i]) {
			i++
		}
		if i == len(line) {
			break
		}
		var value strings.Builder
		var quote byte
		done := false
		for !done && i < len(line) {
			ch := line[i]
			if quote == 0 {
				if persistentSpace(ch) {
					done = true
				} else if ch == '"' || ch == '\'' {
					quote = ch
				} else {
					value.WriteByte(ch)
				}
			} else if ch == quote {
				if i+1 < len(line) && !persistentSpace(line[i+1]) {
					return nil, errors.New("invalid closing quote")
				}
				quote, done = 0, true
			} else if quote == '"' && ch == '\\' && i+1 < len(line) {
				if i+3 < len(line) && line[i+1] == 'x' && persistentHex(line[i+2]) >= 0 && persistentHex(line[i+3]) >= 0 {
					value.WriteByte(byte(persistentHex(line[i+2])*16 + persistentHex(line[i+3])))
					i += 3
				} else {
					i++
					decoded := line[i]
					switch decoded {
					case 'n':
						decoded = '\n'
					case 'r':
						decoded = '\r'
					case 't':
						decoded = '\t'
					case 'b':
						decoded = '\b'
					case 'a':
						decoded = '\a'
					}
					value.WriteByte(decoded)
				}
			} else if quote == '\'' && ch == '\\' && i+1 < len(line) && line[i+1] == '\'' {
				value.WriteByte('\'')
				i++
			} else {
				value.WriteByte(ch)
			}
			i++
		}
		if quote != 0 || !persistentValueSafe(value.String()) {
			return nil, errors.New("invalid decoded configuration value")
		}
		args = append(args, value.String())
	}
	return args, nil
}

func persistentSpace(ch byte) bool { return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' }

func persistentHex(ch byte) int {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0')
	case ch >= 'a' && ch <= 'f':
		return int(ch-'a') + 10
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10
	default:
		return -1
	}
}
