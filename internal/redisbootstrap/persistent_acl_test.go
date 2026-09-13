package redisbootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestPersistentConfigsDefaultACLRewrite(t *testing.T) {
	c := testCluster()
	r, s := persistentFixture(c, 0, 2)
	password := "safe # password\"\\"
	sum := sha256.Sum256([]byte(password))
	acl := "user default on sanitize-payload #" + hex.EncodeToString(sum[:]) + " ~* &* +@all\n"
	s = s + "requirepass \"safe # password\\\"\\\\\"\n"
	if _, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(r+acl), []byte(s+acl)); err != nil {
		t.Fatalf("legitimate default ACL from real rewrite rejected: %v", err)
	}
	for _, line := range []string{
		strings.Replace(acl, "user default", "user another", 1),
		strings.Replace(acl, " on ", " off ", 1),
		strings.Replace(acl, "sanitize-payload", "skip-sanitize-payload", 1),
		strings.Replace(acl, "#"+hex.EncodeToString(sum[:]), "nopass", 1),
		strings.Replace(acl, "#"+hex.EncodeToString(sum[:]), "#"+strings.Repeat("0", 64), 1),
		strings.Replace(acl, "~*", "allkeys", 1),
		strings.Replace(acl, "+@all", "+@all (+@all)", 1),
		acl + acl,
		"aclfile /outside/users.acl\n",
	} {
		for _, target := range []string{"redis", "sentinel"} {
			rr, ss := r, s
			if target == "redis" {
				rr += line
			} else {
				ss += line
			}
			if got, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte(rr), []byte(ss)); err == nil || got != (PersistentConfigSnapshot{}) {
				t.Fatal("unsupported/mismatched rewritten ACL accepted")
			}
		}
	}
	for _, bad := range []string{"", "requirepass \"\"\n", "requirepass correct\nrequirepass correct\n"} {
		if _, err := ParsePersistentConfigs(c, testMember(c, 0), "main", []byte("port 6379\nreplicaof "+c.Members[2]+" 6379\n"+bad+acl), []byte(s)); err == nil {
			t.Fatal("default ACL without unique matching password accepted")
		}
	}
}
