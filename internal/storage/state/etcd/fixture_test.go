package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fixturePort struct {
	HostIP   string
	HostPort string
}

type fixtureMember struct {
	ID              string `json:"Id"`
	State           struct{ Running bool }
	Config          struct{ Labels map[string]string }
	NetworkSettings struct{ Ports map[string][]fixturePort }
}

var fixtureProjectPattern = regexp.MustCompile(`^sandbox-etcd-state-test-[0-9]+-[0-9]+$`)
var fixtureContainerPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

// Global alarms and leader faults require the isolated script fixture. Verify
// every real Docker member and published endpoint before provisioning test keys.
func ownedFixtureContainers(t *testing.T) []string {
	t.Helper()
	project := os.Getenv("TEST_ETCD_FIXTURE_PROJECT")
	if project == "" {
		t.Skip("global faults require the isolated scripts/test-etcd-state.sh fixture")
	}
	ids := strings.Split(os.Getenv("TEST_ETCD_CONTAINERS"), ",")
	endpoints := strings.Split(os.Getenv("TEST_ETCD_ENDPOINTS"), ",")
	require.True(t, fixtureProjectPattern.MatchString(project), "invalid disposable fixture project")
	require.Len(t, ids, 3)
	for _, id := range ids {
		require.True(t, fixtureContainerPattern.MatchString(id), "invalid fixture container ID")
	}
	output, err := runFixtureDocker(append([]string{"inspect"}, ids...)...)
	require.NoError(t, err, string(output))
	var members []fixtureMember
	require.NoError(t, json.Unmarshal(output, &members))
	require.NoError(t, validateFixtureMembers(project, endpoints, ids, members))
	return ids
}

func runFixtureDocker(arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
}

func validateFixtureMembers(project string, endpoints, ids []string, members []fixtureMember) error {
	if !fixtureProjectPattern.MatchString(project) || len(endpoints) != 3 || len(ids) != 3 || len(members) != 3 {
		return fmt.Errorf("invalid isolated fixture inventory")
	}
	services := []string{"etcd-1", "etcd-2", "etcd-3"}
	seen := make(map[string]bool, 3)
	for i, member := range members {
		ports := member.NetworkSettings.Ports["2379/tcp"]
		if !fixtureContainerPattern.MatchString(ids[i]) || !strings.HasPrefix(member.ID, ids[i]) || seen[member.ID] || !member.State.Running || member.Config.Labels["com.docker.compose.project"] != project || member.Config.Labels["com.docker.compose.service"] != services[i] || len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || endpoints[i] != "http://127.0.0.1:"+ports[0].HostPort {
			return fmt.Errorf("member %d is not bound to the owned fixture endpoint", i)
		}
		seen[member.ID] = true
	}
	return nil
}

func TestFixtureMemberOwnershipRejectsUnrelatedCluster(t *testing.T) {
	project := "sandbox-etcd-state-test-123-456"
	ids := []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc"}
	endpoints := []string{"http://127.0.0.1:12301", "http://127.0.0.1:12302", "http://127.0.0.1:12303"}
	makeMembers := func() []fixtureMember {
		members := make([]fixtureMember, 3)
		for i := range members {
			members[i].ID = ids[i]
			members[i].State.Running = true
			members[i].Config.Labels = map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": []string{"etcd-1", "etcd-2", "etcd-3"}[i]}
			members[i].NetworkSettings.Ports = map[string][]fixturePort{"2379/tcp": {{HostIP: "127.0.0.1", HostPort: []string{"12301", "12302", "12303"}[i]}}}
		}
		return members
	}
	require.NoError(t, validateFixtureMembers(project, endpoints, ids, makeMembers()))
	cases := map[string]func([]fixtureMember){
		"foreign project": func(m []fixtureMember) { m[1].Config.Labels["com.docker.compose.project"] = "production" },
		"foreign service": func(m []fixtureMember) { m[1].Config.Labels["com.docker.compose.service"] = "foreign" },
		"wrong port":      func(m []fixtureMember) { m[1].NetworkSettings.Ports["2379/tcp"][0].HostPort = "2379" },
		"external bind":   func(m []fixtureMember) { m[1].NetworkSettings.Ports["2379/tcp"][0].HostIP = "0.0.0.0" },
		"stopped":         func(m []fixtureMember) { m[1].State.Running = false },
		"wrong container": func(m []fixtureMember) { m[1].ID = "dddddddddddd" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			members := makeMembers()
			mutate(members)
			require.Error(t, validateFixtureMembers(project, endpoints, ids, members))
		})
	}
	require.Error(t, validateFixtureMembers("sandbox-etcd-state-production", endpoints, ids, makeMembers()))
	require.Error(t, validateFixtureMembers(project, endpoints, ids[:2], makeMembers()))
}
