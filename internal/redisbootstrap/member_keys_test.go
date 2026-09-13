package redisbootstrap

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMemberKeysRoundTripAndIsolation(t *testing.T) {
	secret, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil || secret == nil || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 4 {
		t.Fatal("missing immutable member key Secret")
	}
	keys, err := ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		private, err := ParseMemberPrivateSeed(secret.Data[fmt.Sprintf("redis-sentinel-%d", i)], keys, i)
		if err != nil || len(private) != ed25519.PrivateKeySize || !bytes.Equal(private.Public().(ed25519.PublicKey), keys[i]) {
			t.Fatal("mismatched member seed")
		}
		if _, err := ParseMemberPrivateSeed(secret.Data[fmt.Sprintf("redis-sentinel-%d", i)], keys, (i+1)%3); err == nil {
			t.Fatal("cross-member seed accepted")
		}
	}
	other, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil || bytes.Equal(secret.Data["public-keys.json"], other.Data["public-keys.json"]) {
		t.Fatal("member keys were reused")
	}
}

func TestMemberKeysRejectsMalformedPublicFiles(t *testing.T) {
	secret, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	var encoded []string
	if err := json.Unmarshal(secret.Data["public-keys.json"], &encoded); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"", "null", "[]", `[null,null,null]`, `[1,2,3]`, `{}`, string(secret.Data["public-keys.json"]) + " {}", strings.Repeat(" ", 1025), string([]byte{0xff})} {
		if _, err := ParseMemberPublicKeys([]byte(data)); err == nil {
			t.Fatal("accepted malformed public file")
		}
	}
	identity := make([]byte, 32)
	identity[0] = 1
	for _, bad := range [][]string{encoded[:2], append(append([]string(nil), encoded...), encoded[0]), {encoded[0], encoded[0], encoded[2]}, {strings.ToUpper(encoded[0]), encoded[1], encoded[2]}, {"00", encoded[1], encoded[2]}, {hex.EncodeToString(identity), encoded[1], encoded[2]}, {strings.Repeat("0", 64), encoded[1], encoded[2]}} {
		data, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseMemberPublicKeys(data); err == nil {
			t.Fatal("accepted invalid or reused public key")
		}
	}
}

func TestMemberKeysRejectsMalformedNamesAndSeeds(t *testing.T) {
	for _, pair := range [][2]string{{"", "redis-sentinel"}, {"isolated", ""}, {"127.0.0.1", "redis-sentinel"}, {"isolated", "BAD"}, {"isolated", strings.Repeat("a", 55)}, {strings.Repeat("a", 64), "redis-sentinel"}} {
		if _, err := GenerateIdentitySecret(pair[0], pair[1]); err == nil {
			t.Fatal("accepted invalid key Secret name")
		}
	}
	secret, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range [][]byte{nil, make([]byte, 31), make([]byte, 33), make([]byte, 64), []byte("do-not-log-seed-not-thirtytwo-bytes")} {
		if _, err := ParseMemberPrivateSeed(seed, keys, 0); err == nil || strings.Contains(err.Error(), "do-not-log") {
			t.Fatal("accepted or leaked malformed seed")
		}
	}
	for _, ordinal := range []int{-1, 3} {
		if _, err := ParseMemberPrivateSeed(secret.Data["redis-sentinel-0"], keys, ordinal); err == nil {
			t.Fatal("accepted invalid member")
		}
	}
	keys[1] = keys[0]
	if _, err := ParseMemberPrivateSeed(secret.Data["redis-sentinel-0"], keys, 0); err == nil {
		t.Fatal("accepted invalid public set")
	}
}
