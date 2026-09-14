package redisbootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainedPartsIndependentMonitorAndRole(t *testing.T) {
	directory, r, keys, member, o := initialTransactionFixture(t, 1)
	if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, o); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(directory, "sentinel.conf")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "sentinel monitor \"sandbox\" \""+r.Cluster.Members[0]+"\"", "sentinel monitor \"sandbox\" \""+r.Cluster.Members[2]+"\"", 1))
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	parts, err := readRetainedParts(context.Background(), directory, r, keys, member, o.MasterName)
	if err != nil {
		t.Fatal("independent retained launch read rejected valid divergent monitor", err)
	}
	if parts.redis.Role != Replica || parts.redis.PrimaryDNS != r.Cluster.Members[0] || parts.sentinel.State.PrimaryDNS != r.Cluster.Members[2] {
		t.Fatal("lost independent state")
	}
	if _, err := ReadLocalVolume(context.Background(), directory, r.Cluster, member, o.MasterName); err == nil {
		t.Fatal("ordinary joint inventory weakened")
	}
}

func TestRetainedPartsRejectsWrongRegisteredMarkerAndSentinelID(t *testing.T) {
	for _, change := range []string{"marker", "id", "mode", "hardlink"} {
		t.Run(change, func(t *testing.T) {
			directory, r, keys, member, o := initialTransactionFixture(t, 1)
			if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, o); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "marker":
				r.MarkerIDs[1] = strings.Repeat("f", 32)
			case "id":
				p := filepath.Join(directory, "sentinel.conf")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(string(b), "\n")
				for i, l := range lines {
					if strings.HasPrefix(l, "sentinel myid ") {
						lines[i] = "sentinel myid " + strings.Repeat("a", 40)
					}
				}
				if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(directory, "redis.conf"), 0640); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(directory, "redis.conf"), filepath.Join(directory, "linked")); err != nil {
					t.Fatal(err)
				}
			}
			parts, err := readRetainedParts(context.Background(), directory, r, keys, member, o.MasterName)
			if err == nil || parts.identity.MarkerID != "" {
				t.Fatal("untrusted retained parts accepted")
			}
		})
	}
}
