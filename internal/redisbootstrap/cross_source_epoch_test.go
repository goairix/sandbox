package redisbootstrap

import "testing"

func TestLowerEpochDiscoveryCannotContradictAnotherPersistedSource(t *testing.T) {
	c := testCluster()
	c.Phase = Initialized
	inventory := retainedInventory(c, 2)
	inventory[0].Persisted.SentinelEpoch = 2
	inventory[0].Persisted.PrimaryDNS = c.Members[0]
	inventory[1].Persisted.SentinelEpoch = 3
	inventory[1].Persisted.PrimaryDNS = c.Members[0]
	evidence := []SentinelEvidence{{Member: testMember(c, 0), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[1], Epoch: 3}, {Member: testMember(c, 1), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[2], Epoch: 4}, {Member: testMember(c, 2), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[2], Epoch: 4}}
	got, err := DecideBootstrap(&c, testMember(c, 2), inventory, evidence)
	if err == nil && got.Action != Wait {
		t.Fatalf("cross-source epoch contradiction restored primary: %+v", got)
	}
}
