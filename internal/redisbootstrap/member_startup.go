package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// MemberStartupOptions describes one fixed member's startup and control mounts.
type MemberStartupOptions struct {
	Files     BootstrapFilePaths
	Directory string
	Ordinal   int
	Initial   InitialConfigOptions
	Timeout   time.Duration
}

var errMemberLaunch = errors.New("member startup is unconfirmed")

type memberStartupDependencies struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

// PrepareMemberLaunch validates retained startup authority and returns a config
// path. It never performs steady-state elections or promotes a retained replica.
func PrepareMemberLaunch(ctx context.Context, o MemberStartupOptions, sentinel bool) (string, error) {
	return prepareMemberLaunch(ctx, o, sentinel, memberStartupDependencies{})
}

func prepareMemberLaunch(ctx context.Context, o MemberStartupOptions, sentinel bool, d memberStartupDependencies) (string, error) {
	if ctx == nil || !validBootstrapPaths(o.Files) || !filepath.IsAbs(o.Directory) || filepath.Clean(o.Directory) == "/" || o.Ordinal < 0 || o.Ordinal > 2 || o.Timeout < 0 || o.Timeout > 10*time.Minute || !validMemberInitialOptions(o.Initial) {
		return "", errMemberLaunch
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		files, err := ReadBootstrapFiles(ctx, o.Files)
		if err == nil {
			member := Member{Ordinal: o.Ordinal, DNS: files.Cluster.Members[o.Ordinal]}
			if files.Registration == nil {
				// This is the sole reservation path. Registered replacements never
				// gain a new marker, even while the API phase remains Pending.
				if files.Cluster.Phase == Pending {
					if _, reserveErr := ReserveLocalVolume(ctx, o.Directory, files.Cluster, member, o.Initial.MasterName); reserveErr != nil && ctx.Err() != nil {
						return "", ctx.Err()
					}
				}
			} else {
				r := *files.Registration
				parts, readErr := readRetainedParts(ctx, o.Directory, r, files.PublicKeys, member, o.Initial.MasterName)
				if readErr != nil && files.Cluster.Phase == Pending {
					// The transaction itself requires the retained registered marker;
					// it cannot reserve a missing or replacement volume.
					if _, err = ConfigureInitialLocalVolume(ctx, o.Directory, r, files.PublicKeys, member, o.Initial); err == nil {
						parts, readErr = readRetainedParts(ctx, o.Directory, r, files.PublicKeys, member, o.Initial.MasterName)
					}
				}
				if readErr == nil {
					var selected *SelectedEvidence
					allowed := sentinel || parts.redis.Role == Replica
					if !allowed {
						evidence, complete := collectStartupEvidence(ctx, r, files.PublicKeys, member, parts, d)
						if complete {
							s, selectErr := SelectEvidence(files.Cluster, evidence)
							if selectErr == nil {
								own := parts.sentinel.State
								allowed = own.PrimaryDNS == s.PrimaryDNS && own.SentinelEpoch == s.Epoch
								if files.Cluster.Phase == Pending && member.Ordinal == 0 && s.Epoch == 0 && s.PrimaryDNS == member.DNS {
									allowed = allowed && len(evidence) == 3
								}
								selected = &s
								if s.PrimaryDNS == member.DNS {
									allowed = allowed && parts.redis.Role == Primary
								}
							}
						}
					}
					if allowed {
						path, launchErr := confirmMemberLaunch(ctx, o, r, files.PublicKeys, member, parts, sentinel, selected)
						if launchErr == nil {
							return path, nil
						}
					}
				}
			}
		}
		if err := waitBootstrapPoll(ctx); err != nil {
			return "", err
		}
	}
}

func validMemberInitialOptions(o InitialConfigOptions) bool {
	return safeMasterName(o.MasterName) && o.DataPassword != o.SentinelPassword && ValidateBuiltinSentinelPassword(o.DataPassword) == nil && ValidateBuiltinSentinelPassword(o.SentinelPassword) == nil && o.DownAfterMilliseconds >= 1000 && o.DownAfterMilliseconds <= 60000 && o.FailoverTimeoutMilliseconds >= 2*o.DownAfterMilliseconds && o.FailoverTimeoutMilliseconds <= 600000 && o.ParallelSyncs >= 1 && o.ParallelSyncs <= 2
}

type startupContact struct {
	proof     IdentityProof
	state     IdentityContactState
	challenge IdentityChallenge
}

func collectStartupEvidence(ctx context.Context, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, parts retainedParts, d memberStartupDependencies) ([]SentinelEvidence, bool) {
	own := parts.sentinel.State
	evidence := []SentinelEvidence{{Member: member, ClusterID: r.Cluster.ClusterID, Persisted: true, PrimaryDNS: own.PrimaryDNS, Epoch: own.SentinelEpoch}}
	results := make(chan startupContact, 2)
	for i := range 3 {
		if i == member.Ordinal {
			continue
		}
		peer := Member{Ordinal: i, DNS: r.Cluster.Members[i]}
		go func() {
			ch, err := NewIdentityChallenge(InventoryProof)
			if err != nil {
				results <- startupContact{}
				return
			}
			expected := VolumeIdentity{ClusterID: r.Cluster.ClusterID, Member: peer, MarkerID: r.MarkerIDs[peer.Ordinal], InitialConfig: Configured}
			proof, state := fetchStartupContact(ctx, r.Cluster, peer, keys[peer.Ordinal], ch, &expected, d)
			results <- startupContact{proof: proof, state: state, challenge: ch}
		}()
	}
	complete := true
	for range 2 {
		contact := <-results
		if contact.state == ContactUnreachable {
			continue
		}
		p := contact.proof
		if contact.state != ContactVerified || VerifyRegisteredProof(r, keys, contact.challenge, p, nil) != nil || p.Observation.Volume.Identity == nil || p.Observation.Volume.Identity.InitialConfig != Configured || p.Observation.Snapshot == nil || p.Observation.Snapshot.SentinelID != fixedSentinelID(keys[p.Member.Ordinal]) {
			complete = false
			continue
		}
		s := p.Observation.Snapshot.State
		evidence = append(evidence, SentinelEvidence{Member: p.Member, ClusterID: r.Cluster.ClusterID, Persisted: true, PrimaryDNS: s.PrimaryDNS, Epoch: s.SentinelEpoch})
	}
	return evidence, complete && ctx.Err() == nil
}

// The private dial seam virtualizes fixed DNS TCP transport only. Every result
// still traverses HTTP parsing, fresh nonce and signature/marker verification.
func fetchStartupContact(ctx context.Context, c ClusterState, m Member, key ed25519.PublicKey, ch IdentityChallenge, expected *VolumeIdentity, d memberStartupDependencies) (IdentityProof, IdentityContactState) {
	client := newIdentityHTTPClient()
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	dial := transport.DialContext
	if d.dial != nil {
		dial = d.dial
	}
	var attempted, connected, failed atomic.Bool
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempted.Store(true)
		conn, err := dial(ctx, network, address)
		if err == nil && conn != nil {
			connected.Store(true)
		} else if err != nil && conn == nil {
			failed.Store(true)
		}
		return conn, err
	}
	p, err := fetchIdentityProof(ctx, client, "http://"+m.DNS+":18080/v1/identity", c, m, key, ch, expected, nil)
	if ctx.Err() != nil {
		return IdentityProof{}, ContactRejected
	}
	if err == nil {
		return p, ContactVerified
	}
	if attempted.Load() && failed.Load() && !connected.Load() {
		return IdentityProof{}, ContactUnreachable
	}
	return IdentityProof{}, ContactRejected
}

func confirmMemberLaunch(ctx context.Context, o MemberStartupOptions, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, before retainedParts, sentinel bool, selected *SelectedEvidence) (path string, resultErr error) {
	return confirmMemberLaunchWithHook(ctx, o, r, keys, member, before, sentinel, selected, nil)
}

// The private checkpoint seam injects failures or file replacement only; it
// cannot manufacture positive evidence or bypass a durability validation.
func confirmMemberLaunchWithHook(ctx context.Context, o MemberStartupOptions, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, before retainedParts, sentinel bool, selected *SelectedEvidence, hook func(string) error) (path string, resultErr error) {
	defer func() {
		if resultErr != nil {
			path = ""
			resultErr = errMemberLaunch
		}
	}()
	root, err := openLocalRoot(ctx, o.Directory, r.Cluster, member)
	if err != nil {
		return "", err
	}
	defer func() {
		if root.Close() != nil {
			resultErr = errMemberLaunch
		}
	}()
	lock, err := acquireInitialLock(ctx, root)
	if err != nil {
		return "", err
	}
	defer func() {
		unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		closeErr := lock.Close()
		if unlockErr != nil || closeErr != nil {
			resultErr = errMemberLaunch
		}
	}()
	if err := validateInitialFileRecords(ctx, root, before.files); err != nil {
		return "", err
	}
	for _, f := range before.files {
		if err := syncInitialFile(ctx, root, f.name, f.data, f.info, nil); err != nil {
			return "", err
		}
	}
	if err := syncInitialDirectory(ctx, root, nil); err != nil {
		return "", err
	}
	if err := validateInitialFileRecords(ctx, root, before.files); err != nil {
		return "", err
	}
	if !sentinel && selected != nil && selected.PrimaryDNS != member.DNS {
		if before.redis.Role != Primary || before.sentinel.State.PrimaryDNS != selected.PrimaryDNS || before.sentinel.State.SentinelEpoch != selected.Epoch {
			return "", errMemberLaunch
		}
		// A prior primary has no replicaof directive. Preserve every original
		// byte and append only the fixed-DNS replica target; never touch Sentinel.
		data := bytes.Clone(before.files[1].data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		data = append(data, []byte("replicaof "+selected.PrimaryDNS+" 6379\n")...)
		if _, err := parsePersistentRedis(r.Cluster, member, data); err != nil {
			return "", errMemberLaunch
		}
		temp, syncedInfo, err := initialPrivateTemp(ctx, root, ".bootstrap-follow-", "redis.conf", data, nil)
		if err != nil {
			return "", err
		}
		defer func() {
			if resultErr == nil {
				return
			}
			actual, info, readErr := readPrivateLocalFile(context.WithoutCancel(ctx), root, temp, int64(len(data)))
			if readErr == nil && os.SameFile(syncedInfo, info) && bytes.Equal(actual, data) {
				if root.Remove(temp) != nil {
					resultErr = errMemberLaunch
				}
			}
		}()
		if err := initialCheckpoint(ctx, hook, "follow-temp-synced"); err != nil {
			return "", err
		}
		if err := validateInitialFileRecords(ctx, root, before.files); err != nil {
			return "", err
		}
		if err := initialCheckpoint(ctx, hook, "follow-before-rename"); err != nil {
			return "", err
		}
		if err := validateInitialFileRecords(ctx, root, before.files); err != nil {
			return "", err
		}
		if err := unchangedLocalFile(ctx, root, temp, int64(len(data)), data, syncedInfo); err != nil {
			return "", err
		}
		if err := root.Rename(temp, "redis.conf"); err != nil {
			return "", err
		}
		before.files[1] = initialRetainedFile{"redis.conf", data, syncedInfo, maximumPersistentConfigBytes}
		if err := initialCheckpoint(ctx, hook, "follow-renamed"); err != nil {
			return "", err
		}
		if err := syncInitialDirectory(ctx, root, nil); err != nil {
			return "", err
		}
		if err := validateInitialFileRecords(ctx, root, before.files); err != nil {
			return "", err
		}
	}
	if err := initialCheckpoint(ctx, hook, "member-before-return"); err != nil {
		return "", err
	}
	if err := validateInitialFileRecords(ctx, root, before.files); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name := "redis.conf"
	if sentinel {
		name = "sentinel.conf"
	}
	return filepath.Join(filepath.Clean(o.Directory), name), nil
}
