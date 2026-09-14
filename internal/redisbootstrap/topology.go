package redisbootstrap

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

type TopologyOptions struct {
	Registration                               BootstrapRegistration
	PublicKeys                                 [3]ed25519.PublicKey
	MasterName, DataPassword, SentinelPassword string
	AckTimeout                                 time.Duration
}

type topologyDependencies struct {
	dial  func(context.Context, string, string) (net.Conn, error)
	fetch func(context.Context, ClusterState, Member, ed25519.PublicKey, IdentityChallenge, *VolumeIdentity, *AuthenticatedEndpoint) (IdentityProof, IdentityContactState, error)
}

var errTopology = errors.New("redis bootstrap topology is unconfirmed")

func VerifyTopology(ctx context.Context, o TopologyOptions, all bool) error {
	return verifyTopology(ctx, o, all, topologyDependencies{
		dial:  (&net.Dialer{Timeout: time.Second}).DialContext,
		fetch: FetchIdentityProofContact,
	})
}

// This is a bounded installation/upgrade check, never a steady-state election
// loop. A successful WAIT is replication acknowledgement, not a consensus or
// zero-data-loss guarantee. The only write is a short-lived random barrier.
func verifyTopology(ctx context.Context, o TopologyOptions, all bool, dependencies topologyDependencies) (resultErr error) {
	if ctx == nil || !validTopologyOptions(o) || dependencies.dial == nil || dependencies.fetch == nil {
		return errTopology
	}
	caller := ctx
	defer func() {
		if resultErr != nil {
			if caller.Err() != nil {
				resultErr = caller.Err()
			} else {
				resultErr = errTopology
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second+o.AckTimeout)
	defer cancel()
	if o.Registration.Cluster.Phase == Pending {
		all = true
	}
	inventory, selected, err := readTopologyInventory(ctx, o, all, dependencies)
	if err != nil {
		return err
	}
	var members [3]*topologyMember
	defer func() {
		for _, m := range members {
			if m != nil && m.close() != nil {
				resultErr = errTopology
			}
		}
	}()
	type observed struct {
		ordinal     int
		member      *topologyMember
		unreachable bool
		err         error
	}
	results := make(chan observed, 3)
	count := 0
	for i, p := range inventory {
		if p == nil {
			continue
		}
		count++
		go func(i int, p *IdentityProof) {
			m, unreachable, err := readTopologyMember(ctx, o, i, p, selected, dependencies)
			results <- observed{i, m, unreachable, err}
		}(i, p)
	}
	var failure error
	for range count {
		r := <-results
		members[r.ordinal] = r.member
		if r.err != nil && (all || !r.unreachable) {
			failure = errTopology
		}
	}
	if failure != nil || ctx.Err() != nil {
		return errTopology
	}
	primary := -1
	healthy := 0
	processes := map[string]bool{}
	for i, m := range members {
		if m == nil || !m.healthy {
			continue
		}
		healthy++
		if processes[m.data.runID] {
			return errTopology
		}
		processes[m.data.runID] = true
		if m.data.role == Primary {
			if primary != -1 {
				return errTopology
			}
			primary = i
		}
	}
	if healthy < 2 || (all && healthy != 3) || primary < 0 || o.Registration.Cluster.Members[primary] != selected.PrimaryDNS {
		return errTopology
	}
	master := members[primary]
	for _, m := range members {
		if m == nil || !m.healthy {
			continue
		}
		if m.data.replID != master.data.replID || m.monitorRunID != master.data.runID {
			return errTopology
		}
		if m.data.role == Replica && (m.data.primary != selected.PrimaryDNS || !master.data.replicas[m.member.DNS]) {
			return errTopology
		}
	}
	nonce, err := NewProofSession()
	if err != nil {
		return errTopology
	}
	if err := master.redis.conn.Do(ctx, "SET", "sandbox:bootstrap-barrier:"+nonce, "1", "PX", 60000).Err(); err != nil {
		return errTopology
	}
	acks, err := master.redis.conn.Wait(ctx, 1, o.AckTimeout).Result()
	if err != nil || acks < 1 {
		return errTopology
	}
	// Use the same authenticated connections again. A replacement process, role
	// change or inconsistent replication lineage invalidates the previous proofs.
	for _, m := range members {
		if m == nil || !m.healthy {
			continue
		}
		info, err := m.redis.conn.Info(ctx, "server", "replication").Result()
		if err != nil {
			return errTopology
		}
		current, err := parseTopologyINFO(o.Registration.Cluster, m.member, info)
		if err != nil || current.runID != m.data.runID || current.role != m.data.role || current.primary != m.data.primary || current.replID != m.data.replID {
			return errTopology
		}
	}
	return ctx.Err()
}

func validTopologyOptions(o TopologyOptions) bool {
	digest, err := PublicKeySetDigest(o.PublicKeys)
	return o.Registration.Validate() == nil && err == nil && digest == o.Registration.KeyDigest && safeMasterName(o.MasterName) && ValidateBuiltinSentinelPassword(o.DataPassword) == nil && ValidateBuiltinSentinelPassword(o.SentinelPassword) == nil && o.DataPassword != o.SentinelPassword && o.AckTimeout >= time.Second && o.AckTimeout <= 10*time.Second
}

func readTopologyInventory(ctx context.Context, o TopologyOptions, all bool, d topologyDependencies) ([3]*IdentityProof, SelectedEvidence, error) {
	var proofs [3]*IdentityProof
	type reply struct {
		i     int
		proof IdentityProof
		state IdentityContactState
		err   error
	}
	results := make(chan reply, 3)
	for i := range 3 {
		go func(i int) {
			ch, err := NewIdentityChallenge(InventoryProof)
			if err != nil {
				results <- reply{i: i, err: err}
				return
			}
			member := fixedTopologyMember(o.Registration.Cluster, i)
			expected := topologyExpectedIdentity(o.Registration, i)
			p, state, err := d.fetch(ctx, o.Registration.Cluster, member, o.PublicKeys[i], ch, &expected, nil)
			if err == nil && (state != ContactVerified || VerifyRegisteredProof(o.Registration, o.PublicKeys, ch, p, nil) != nil || p.Observation.Volume.Identity == nil || p.Observation.Volume.Identity.InitialConfig != Configured || p.Observation.Snapshot == nil || p.Observation.Snapshot.SentinelID != topologySentinelID(o.PublicKeys[i])) {
				state = ContactRejected
				err = errTopology
			}
			results <- reply{i, p, state, err}
		}(i)
	}
	var evidence []SentinelEvidence
	var failure error
	for range 3 {
		r := <-results
		if r.err != nil {
			if all || r.state != ContactUnreachable {
				failure = errTopology
			}
			continue
		}
		proofs[r.i] = &r.proof
		state := r.proof.Observation.Snapshot.State
		evidence = append(evidence, SentinelEvidence{Member: state.Member, ClusterID: o.Registration.Cluster.ClusterID, Persisted: true, PrimaryDNS: state.PrimaryDNS, Epoch: state.SentinelEpoch})
	}
	if failure != nil || ctx.Err() != nil {
		return proofs, SelectedEvidence{}, errTopology
	}
	selected, err := SelectEvidence(o.Registration.Cluster, evidence)
	if err != nil {
		return proofs, SelectedEvidence{}, errTopology
	}
	return proofs, selected, nil
}

func fixedTopologyMember(c ClusterState, i int) Member { return Member{DNS: c.Members[i], Ordinal: i} }
func topologyExpectedIdentity(r BootstrapRegistration, i int) VolumeIdentity {
	return VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[i], Member: fixedTopologyMember(r.Cluster, i), InitialConfig: Configured}
}
func topologySentinelID(key ed25519.PublicKey) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:20])
}

type topologyConnection struct {
	client            *redis.Client
	conn              *redis.Conn
	connected, failed atomic.Bool
	attempted         atomic.Bool
}

func newTopologyConnection(address, password string, d topologyDependencies) *topologyConnection {
	c := &topologyConnection{}
	c.client = redis.NewClient(&redis.Options{Addr: address, Password: password, Protocol: 2, DisableIdentity: true, MaxRetries: -1, DialerRetries: 1, DialerRetryTimeout: time.Nanosecond, PoolSize: 1, MaxActiveConns: 1, ContextTimeoutEnabled: true, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
		// MaxRetries does not disable pool dial retries/background probes in
		// go-redis. Only this first invocation can perform actual network I/O;
		// a lost socket never silently substitutes a different process here.
		if !c.attempted.CompareAndSwap(false, true) || ctx.Err() != nil {
			return nil, errTopology
		}
		conn, err := d.dial(ctx, network, address)
		if err != nil || conn == nil {
			if err != nil && conn == nil {
				c.failed.Store(true)
			}
			return nil, errTopology
		}
		c.connected.Store(true)
		return &localRESPConn{Conn: conn, reader: bufio.NewReaderSize(conn, 1024)}, nil
	}})
	c.conn = c.client.Conn()
	return c
}
func (c *topologyConnection) unreachable() bool { return c.failed.Load() && !c.connected.Load() }
func (c *topologyConnection) close() error      { return errors.Join(c.conn.Close(), c.client.Close()) }

type topologyMember struct {
	member          Member
	redis, sentinel *topologyConnection
	data            topologyData
	monitorRunID    string
	healthy         bool
}

func (m *topologyMember) close() error { return errors.Join(m.redis.close(), m.sentinel.close()) }

func readTopologyMember(ctx context.Context, o TopologyOptions, i int, inventory *IdentityProof, selected SelectedEvidence, d topologyDependencies) (out *topologyMember, unreachable bool, resultErr error) {
	member := fixedTopologyMember(o.Registration.Cluster, i)
	out = &topologyMember{member: member, redis: newTopologyConnection(member.DNS+":6379", o.DataPassword, d), sentinel: newTopologyConnection(member.DNS+":26379", o.SentinelPassword, d)}
	defer func() {
		if resultErr != nil && ctx.Err() == nil {
			unreachable = out.redis.unreachable() || out.sentinel.unreachable()
		}
	}()
	info, err := out.redis.conn.Info(ctx, "server", "replication").Result()
	if err != nil {
		return out, false, errTopology
	}
	out.data, err = parseTopologyINFO(o.Registration.Cluster, member, info)
	if err != nil {
		return out, false, errTopology
	}
	for key, expected := range map[string]string{"appendonly": "yes", "appendfsync": "everysec", "min-replicas-to-write": "1", "min-replicas-max-lag": "10"} {
		actual, err := out.redis.conn.ConfigGet(ctx, key).Result()
		if err != nil || len(actual) != 1 || actual[key] != expected {
			return out, false, errTopology
		}
	}
	id, err := out.sentinel.conn.Do(ctx, "SENTINEL", "MYID").Text()
	if err != nil || id != topologySentinelID(o.PublicKeys[i]) {
		return out, false, errTopology
	}
	reply, err := out.sentinel.conn.Do(ctx, "SENTINEL", "MASTER", o.MasterName).Result()
	if err != nil {
		return out, false, errTopology
	}
	monitor, err := topologyMap(reply)
	if err != nil || !validTopologyMonitor(o, monitor, selected) {
		return out, false, errTopology
	}
	out.monitorRunID = monitor["runid"]
	reply, err = out.sentinel.conn.Do(ctx, "SENTINEL", "SENTINELS", o.MasterName).Result()
	if err != nil || !validTopologyPeers(o, i, reply) {
		return out, false, errTopology
	}
	quorum, err := out.sentinel.conn.Do(ctx, "SENTINEL", "CKQUORUM", o.MasterName).Text()
	if err != nil || !strings.HasPrefix(quorum, "OK ") {
		return out, false, errTopology
	}
	ch, err := NewIdentityChallenge(LiveProof)
	if err != nil {
		return out, false, errTopology
	}
	expected := topologyExpectedIdentity(o.Registration, i)
	current := &AuthenticatedEndpoint{Member: member, RunID: out.data.runID, Authenticated: true}
	live, state, err := d.fetch(ctx, o.Registration.Cluster, member, o.PublicKeys[i], ch, &expected, current)
	if err != nil || state != ContactVerified || VerifyRegisteredProof(o.Registration, o.PublicKeys, ch, live, current) != nil || live.Observation.Snapshot == nil {
		return out, false, errTopology
	}
	persisted := live.Observation.Snapshot
	prior := inventory.Observation.Snapshot
	if persisted.State != prior.State || persisted.SentinelID != id || persisted.CurrentEpoch != prior.CurrentEpoch || live.Observation.ConfigDigest != inventory.Observation.ConfigDigest || persisted.State.Role != out.data.role || persisted.State.PrimaryDNS != selected.PrimaryDNS || persisted.State.SentinelEpoch != selected.Epoch {
		return out, false, errTopology
	}
	out.healthy = true
	return out, false, nil
}

type topologyData struct {
	runID, replID, primary string
	role                   Role
	replicas               map[string]bool
}

func parseTopologyINFO(c ClusterState, m Member, info string) (topologyData, error) {
	var out topologyData
	id, err := parseLocalRunID(info)
	if err != nil {
		return out, errTopology
	}
	fields := map[string]string{}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || len(key) > 128 {
			return out, errTopology
		}
		if _, exists := fields[key]; exists {
			return out, errTopology
		}
		fields[key] = value
	}
	out.runID, out.replID = id, fields["master_replid"]
	if !runIDPattern.MatchString(out.replID) || out.replID == strings.Repeat("0", 40) {
		return out, errTopology
	}
	switch fields["role"] {
	case "slave":
		if !containsDNS(c, fields["master_host"]) || fields["master_host"] == m.DNS || fields["master_port"] != "6379" || fields["master_link_status"] != "up" || fields["master_sync_in_progress"] != "0" || !topologyUint(fields["slave_repl_offset"]) {
			return out, errTopology
		}
		out.role, out.primary = Replica, fields["master_host"]
	case "master":
		count, err := strconv.Atoi(fields["connected_slaves"])
		if err != nil || count < 1 || count > 2 || !topologyUint(fields["master_repl_offset"]) {
			return out, errTopology
		}
		out.role, out.primary, out.replicas = Primary, m.DNS, map[string]bool{}
		for n := range count {
			parts := strings.Split(fields["slave"+strconv.Itoa(n)], ",")
			row := map[string]string{}
			for _, part := range parts {
				k, v, ok := strings.Cut(part, "=")
				if !ok || row[k] != "" {
					return out, errTopology
				}
				row[k] = v
			}
			lag, err := strconv.Atoi(row["lag"])
			if len(row) != 5 || !containsDNS(c, row["ip"]) || row["ip"] == m.DNS || out.replicas[row["ip"]] || row["port"] != "6379" || row["state"] != "online" || !topologyUint(row["offset"]) || err != nil || lag < 0 || lag > 10 {
				return out, errTopology
			}
			out.replicas[row["ip"]] = true
		}
	default:
		return out, errTopology
	}
	return out, nil
}

func topologyUint(value string) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == value
}
func topologyMap(reply any) (map[string]string, error) {
	values, ok := reply.([]interface{})
	if !ok || len(values) == 0 || len(values) > 64 || len(values)%2 != 0 {
		return nil, errTopology
	}
	fields := map[string]string{}
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok || key == "" || len(key) > 128 {
			return nil, errTopology
		}
		value, ok := values[i+1].(string)
		if !ok || len(value) > 1024 {
			return nil, errTopology
		}
		if _, exists := fields[key]; exists {
			return nil, errTopology
		}
		fields[key] = value
	}
	return fields, nil
}
func validTopologyMonitor(o TopologyOptions, m map[string]string, s SelectedEvidence) bool {
	return m["name"] == o.MasterName && m["ip"] == s.PrimaryDNS && m["port"] == "6379" && runIDPattern.MatchString(m["runid"]) && m["flags"] == "master" && m["role-reported"] == "master" && m["config-epoch"] == strconv.FormatUint(s.Epoch, 10) && m["num-slaves"] == "2" && m["num-other-sentinels"] == "2" && m["quorum"] == "2" && topologyFreshMillis(m["last-ok-ping-reply"])
}
func topologyFreshMillis(value string) bool {
	n, err := strconv.ParseUint(value, 10, 32)
	return err == nil && n <= 10000 && strconv.FormatUint(n, 10) == value
}
func validTopologyPeers(o TopologyOptions, i int, reply any) bool {
	peers, ok := reply.([]interface{})
	if !ok || len(peers) != 2 {
		return false
	}
	seen := map[string]bool{}
	healthy := 0
	for _, row := range peers {
		p, err := topologyMap(row)
		if err != nil || !containsDNS(o.Registration.Cluster, p["ip"]) || p["ip"] == o.Registration.Cluster.Members[i] || p["port"] != "26379" || seen[p["ip"]] {
			return false
		}
		ordinal := -1
		for n, dns := range o.Registration.Cluster.Members {
			if dns == p["ip"] {
				ordinal = n
			}
		}
		if ordinal < 0 || p["runid"] != topologySentinelID(o.PublicKeys[ordinal]) || p["name"] != p["runid"] {
			return false
		}
		seen[p["ip"]] = true
		if p["flags"] == "sentinel" && topologyFreshMillis(p["last-hello-message"]) {
			healthy++
		}
	}
	return healthy >= 1
}
