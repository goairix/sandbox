package redisbootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var errMemberKeys = errors.New("invalid Redis bootstrap member keys")

// GenerateIdentitySecret creates (but does not install) three independent member keys.
func GenerateIdentitySecret(namespace, statefulSetName string) (*corev1.Secret, error) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Label(statefulSetName)) != 0 || len(statefulSetName) > 54 {
		return nil, errMemberKeys
	}
	immutable := true
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: statefulSetName + "-identity", Namespace: namespace}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: make(map[string][]byte, 4)}
	var keys [3]ed25519.PublicKey
	var encoded [3]string
	for i := range 3 {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, errors.New("member key randomness is unavailable")
		}
		secret.Data[fmt.Sprintf("%s-%d", statefulSetName, i)] = append([]byte(nil), private.Seed()...)
		keys[i] = public
		encoded[i] = hex.EncodeToString(public)
		// The returned Secret intentionally owns the seeds. Discard the temporary
		// expanded signing key rather than retaining an extra copy in this scope.
		for j := range private {
			private[j] = 0
		}
	}
	if _, err := PublicKeySetDigest(keys); err != nil {
		return nil, errMemberKeys
	}
	publicData, err := json.Marshal(encoded)
	if err != nil {
		return nil, errMemberKeys
	}
	secret.Data["public-keys.json"] = publicData
	return secret, nil
}

// ParseMemberPublicKeys reads the public-only fixed three-member key set.
func ParseMemberPublicKeys(data []byte) ([3]ed25519.PublicKey, error) {
	var keys [3]ed25519.PublicKey
	if len(data) == 0 || len(data) > 1024 || !utf8.Valid(data) || checkJSON(data) != nil {
		return keys, errMemberKeys
	}
	var encoded []string
	if json.Unmarshal(data, &encoded) != nil || len(encoded) != 3 {
		return keys, errMemberKeys
	}
	for i, value := range encoded {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != ed25519.PublicKeySize || hex.EncodeToString(decoded) != value {
			return [3]ed25519.PublicKey{}, errMemberKeys
		}
		keys[i] = ed25519.PublicKey(decoded)
	}
	if _, err := PublicKeySetDigest(keys); err != nil {
		return [3]ed25519.PublicKey{}, errMemberKeys
	}
	return keys, nil
}

// ParseMemberPrivateSeed binds one raw member seed to its expected public key.
func ParseMemberPrivateSeed(seed []byte, keys [3]ed25519.PublicKey, ordinal int) (ed25519.PrivateKey, error) {
	if len(seed) != ed25519.SeedSize || ordinal < 0 || ordinal >= 3 {
		return nil, errMemberKeys
	}
	if _, err := PublicKeySetDigest(keys); err != nil {
		return nil, errMemberKeys
	}
	private := ed25519.NewKeyFromSeed(seed)
	if subtle.ConstantTimeCompare(private[32:], keys[ordinal]) != 1 {
		for i := range private {
			private[i] = 0
		}
		return nil, errMemberKeys
	}
	return private, nil
}
