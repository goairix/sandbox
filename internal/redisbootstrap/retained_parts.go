package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
)

var errRetainedParts = errors.New("retained member configuration is unconfirmed")

type retainedParts struct {
	identity VolumeIdentity
	redis    PersistedState
	sentinel PersistentConfigSnapshot
	files    []initialRetainedFile
}

func fixedSentinelID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:20])
}

// readRetainedParts deliberately separates role and monitor parsing. Ordinary
// inventory readers retain the stricter joint role/monitor validation.
func readRetainedParts(ctx context.Context, directory string, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, masterName string) (out retainedParts, resultErr error) {
	defer func() {
		if resultErr != nil {
			out = retainedParts{}
			if ctx.Err() != nil {
				resultErr = ctx.Err()
			} else {
				resultErr = errRetainedParts
			}
		}
	}()
	digest, err := PublicKeySetDigest(keys)
	if r.Validate() != nil || member.Validate(r.Cluster) != nil || err != nil || digest != r.KeyDigest {
		return out, errRetainedParts
	}
	root, err := openLocalRoot(ctx, directory, r.Cluster, member)
	if err != nil {
		return out, err
	}
	defer func() {
		if root.Close() != nil {
			resultErr = errRetainedParts
		}
	}()
	return readRetainedPartsRoot(ctx, root, r, keys, member, masterName)
}

func readRetainedPartsRoot(ctx context.Context, root *os.Root, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, masterName string) (out retainedParts, resultErr error) {
	defer func() {
		if resultErr != nil {
			out = retainedParts{}
		}
	}()
	for _, target := range []struct {
		name string
		max  int64
	}{{"identity.json", maximumStateBytes}, {"redis.conf", maximumPersistentConfigBytes}, {"sentinel.conf", maximumPersistentConfigBytes}} {
		data, info, err := readPrivateLocalFile(ctx, root, target.name, target.max)
		if err != nil {
			return out, errRetainedParts
		}
		out.files = append(out.files, initialRetainedFile{target.name, data, info, target.max})
	}
	identity, err := ParseVolumeIdentity(out.files[0].data, r.Cluster)
	if err != nil || identity.Member != member || identity.InitialConfig != Configured || identity.MarkerID != r.MarkerIDs[member.Ordinal] {
		return out, errRetainedParts
	}
	redis, err := parsePersistentRedis(r.Cluster, member, out.files[1].data)
	if err != nil {
		return out, errRetainedParts
	}
	sentinel, err := parsePersistentSentinel(r.Cluster, member, masterName, out.files[2].data)
	if err != nil || sentinel.SentinelID != fixedSentinelID(keys[member.Ordinal]) {
		return out, errRetainedParts
	}
	if err := validateInitialFileRecords(ctx, root, out.files); err != nil {
		return out, errRetainedParts
	}
	out.identity, out.redis, out.sentinel = identity, redis, sentinel
	return out, nil
}
