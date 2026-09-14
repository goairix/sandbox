package redisbootstrap

import (
	"fmt"
	"net"

	api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// IdentityClusterAnnotation binds newly allocated PVCs to this install only.
const IdentityClusterAnnotation = "sandbox/redis-cluster-id"

// Validate rejects ambiguous scope before any credentials or API are accessed.
func (o IdentitySecretOptions) Validate() error {
	if len(validation.IsDNS1123Label(o.Namespace)) != 0 || len(validation.IsDNS1123Label(o.StatefulSetName)) != 0 || len(o.StatefulSetName) > 54 || len(validation.IsDNS1123Subdomain(o.SecretName)) != 0 || net.ParseIP(o.SecretName) != nil || len(validation.IsDNS1123Subdomain(o.StateConfigMap)) != 0 || net.ParseIP(o.StateConfigMap) != nil {
		return ErrIdentityInvalid
	}
	c := ClusterState{ClusterID: "validation", Members: o.Members, Phase: Pending}
	if c.Validate() != nil || o.Timeout < 0 || o.Timeout > identityTimeout {
		return ErrIdentityInvalid
	}
	if o.FreshClusterID != "" && (!clusterIDPattern.MatchString(o.FreshClusterID) || o.SecretName != o.StatefulSetName+"-identity") {
		return ErrIdentityInvalid
	}
	return nil
}

func validateServerIdentity(s *api.Secret, o IdentitySecretOptions, state NamespaceBootstrap) error {
	if s == nil || s.Name != o.SecretName || s.Namespace != o.Namespace || s.UID == "" || s.ResourceVersion == "" || s.DeletionTimestamp != nil || s.Type != api.SecretTypeOpaque || s.Immutable == nil || !*s.Immutable || len(s.Data) != 4 || len(s.StringData) != 0 {
		return ErrIdentityInvalid
	}
	keys, err := ParseMemberPublicKeys(s.Data["public-keys.json"])
	if err != nil {
		return ErrIdentityInvalid
	}
	for i := range 3 {
		private, err := ParseMemberPrivateSeed(s.Data[fmt.Sprintf("%s-%d", o.StatefulSetName, i)], keys, i)
		if err != nil {
			return ErrIdentityInvalid
		}
		for j := range private {
			private[j] = 0
		}
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil || (state.Registration != nil && state.Registration.KeyDigest != digest) {
		return ErrIdentityInvalid
	}
	return nil
}

func wipeIdentitySeeds(s *api.Secret) {
	if s == nil {
		return
	}
	for key, seed := range s.Data {
		if key == "public-keys.json" {
			continue
		}
		for i := range seed {
			seed[i] = 0
		}
	}
}
