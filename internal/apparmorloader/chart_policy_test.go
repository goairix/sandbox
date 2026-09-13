package apparmorloader

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/mounter"
)

func TestChartPolicyMatchesLoaderContract(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/sandbox/files/apparmor/workspace-mounter.profile")
	if err != nil {
		t.Fatal(err)
	}
	canonical := CanonicalPolicy(raw)
	if strings.Count(string(canonical), ProfilePlaceholder) != 1 {
		t.Fatal("expected one placeholder")
	}
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	name := "sandbox-fuse-" + digest
	rendered := strings.Replace(string(canonical), ProfilePlaceholder, name, 1)
	if err := ValidatePolicy([]byte(rendered), name, digest); err != nil {
		t.Fatal(err)
	}
	for id, profile := range mounter.CompiledProfiles().(mounter.StaticProfiles) {
		argv := profile.Flush(fuseprotocol.BootstrapConfig{MountPath: "/workspace"})
		if len(argv) == 0 {
			t.Fatalf("%s: flush command missing", id)
		}
		if argv[0] != "/bin/sync" {
			t.Fatalf("%s: new flush executable requires policy review: %s", id, argv[0])
		}
		if !strings.Contains(rendered, "/bin/{ls,cat,sync} rix,") || !strings.Contains(rendered, "/usr/bin/{s3fs,fusermount3,ls,cat,sync} rix,") {
			t.Fatalf("%s: durable flush executable missing from inherited AppArmor permissions", id)
		}
	}
}
