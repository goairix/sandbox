package redisbootstrap

import "testing"

func freshInventory() [3]VolumeState {
	return [3]VolumeState{{Empty: true}, {Empty: true}, {Empty: true}}
}

func retainedInventory(c ClusterState, primary int) [3]VolumeState {
	var out [3]VolumeState
	for i := range out {
		v := testIdentity(c, i)
		role := Replica
		if i == primary {
			role = Primary
		}
		out[i] = VolumeState{Identity: &v, Persisted: &PersistedState{Member: testMember(c, i), Role: role, PrimaryDNS: c.Members[primary], SentinelEpoch: 4}}
	}
	return out
}

func testEvidence(c ClusterState, primary int) []SentinelEvidence {
	return []SentinelEvidence{{Member: testMember(c, 0), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[primary], Epoch: 4}, {Member: testMember(c, 1), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[primary], Epoch: 4}}
}

func TestBootstrapDecisions(t *testing.T) {
	for _, name := range []string{"fresh primary", "fresh replica", "missing namespace", "nonempty orphan", "mismatch", "partial pending replica", "partial pending primary", "partial pending primary evidence", "partial marker", "initialized no evidence", "restore nonzero primary", "old primary follows", "replacement follows", "two lost states", "replica never promoted", "conflicting role", "foreign persisted member", "empty retained identity"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			cp := &c
			member := testMember(c, 0)
			inventory := freshInventory()
			var evidence []SentinelEvidence
			want := Wait
			wantErr := false
			switch name {
			case "fresh primary":
				want = SeedPrimary
			case "fresh replica":
				member = testMember(c, 1)
				want = SeedReplica
			case "missing namespace":
				cp = nil
				wantErr = true
			case "nonempty orphan":
				inventory[0].Empty = false
				wantErr = true
			case "mismatch":
				inventory = retainedInventory(c, 2)
				inventory[0].Identity.ClusterID = "other"
				wantErr = true
			case "partial pending replica":
				inventory = retainedInventory(c, 2)
				want = ResumePersisted
			case "partial pending primary":
				inventory = retainedInventory(c, 0)
			case "partial pending primary evidence":
				member = testMember(c, 2)
				inventory = retainedInventory(c, 2)
				evidence = testEvidence(c, 2)
				want = RestorePriorPrimary
			case "partial marker":
				v := testIdentity(c, 0)
				v.InitialConfig = Reserved
				inventory[0] = VolumeState{Identity: &v}
			case "initialized no evidence":
				c.Phase = Initialized
				inventory = retainedInventory(c, 2)
			case "restore nonzero primary":
				c.Phase = Initialized
				member = testMember(c, 2)
				inventory = retainedInventory(c, 2)
				evidence = testEvidence(c, 2)
				want = RestorePriorPrimary
			case "old primary follows":
				c.Phase = Initialized
				inventory = retainedInventory(c, 0)
				evidence = testEvidence(c, 2)
				for i := range evidence {
					evidence[i].Epoch = 5
				}
				want = FollowPrimary
			case "replacement follows":
				c.Phase = Initialized
				inventory = retainedInventory(c, 2)
				inventory[0] = VolumeState{Empty: true}
				evidence = testEvidence(c, 2)
				evidence[0].Member = testMember(c, 2)
				want = FollowPrimary
			case "two lost states":
				c.Phase = Initialized
				inventory = retainedInventory(c, 2)
				inventory[0] = VolumeState{Empty: true}
				inventory[1] = VolumeState{Empty: true}
				evidence = testEvidence(c, 2)[:1]
				evidence[0].Member = testMember(c, 2)
			case "replica never promoted":
				c.Phase = Initialized
				inventory = retainedInventory(c, 2)
				evidence = testEvidence(c, 0)
				wantErr = true
			case "conflicting role":
				inventory = retainedInventory(c, 2)
				inventory[0].Persisted.Role = "bad"
				wantErr = true
			case "foreign persisted member":
				inventory = retainedInventory(c, 2)
				inventory[0].Persisted.Member = testMember(c, 1)
				wantErr = true
			case "empty retained identity":
				inventory = retainedInventory(c, 2)
				inventory[0].Empty = true
				wantErr = true
			}
			got, err := DecideBootstrap(cp, member, inventory, evidence)
			if (err != nil) != wantErr || (!wantErr && got.Action != want) {
				t.Fatalf("got %+v err=%v, want %s err=%v", got, err, want, wantErr)
			}
			if !wantErr && (want == RestorePriorPrimary || want == FollowPrimary) && got.PrimaryDNS != c.Members[2] {
				t.Fatalf("wrong primary: %+v", got)
			}
		})
	}
}

func TestSameEpochPrimaryMappingConflictsReject(t *testing.T) {
	for _, name := range []string{"source retained contradicts live observation", "lower epoch source contradicts retained observation", "unreported retained contradicts selected majority", "retained lower epoch internally contradicts"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			c.Phase = Initialized
			inventory := retainedInventory(c, 2)
			evidence := testEvidence(c, 2)
			switch name {
			case "source retained contradicts live observation":
				inventory = retainedInventory(c, 0)
			case "lower epoch source contradicts retained observation":
				inventory[0].Persisted.SentinelEpoch = 3
				evidence[0].Epoch = 3
				evidence[0].PrimaryDNS = c.Members[1]
				evidence = append(evidence, SentinelEvidence{Member: testMember(c, 2), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[2], Epoch: 4})
			case "unreported retained contradicts selected majority":
				inventory[2].Persisted.Role = Replica
				inventory[2].Persisted.PrimaryDNS = c.Members[1]
			case "retained lower epoch internally contradicts":
				inventory[0].Persisted.SentinelEpoch = 3
				inventory[1].Persisted.SentinelEpoch = 3
				inventory[1].Persisted.PrimaryDNS = c.Members[1]
			}
			got, err := DecideBootstrap(&c, testMember(c, 0), inventory, evidence)
			if err == nil && got.Action != Wait {
				t.Fatalf("same epoch contradiction authorized %+v", got)
			}
		})
	}
}

func TestIncompleteInventoriesNeverReseed(t *testing.T) {
	c := testCluster()
	for mask := 1; mask < 27; mask++ {
		t.Run(string(rune('A'+mask)), func(t *testing.T) {
			inventory := freshInventory()
			states := mask
			for i := range inventory {
				state := states % 3
				states /= 3
				if state == 0 {
					continue
				}
				v := testIdentity(c, i)
				inventory[i] = VolumeState{Identity: &v}
				if state == 1 {
					v.InitialConfig = Reserved
				} else {
					role := Replica
					if i == 0 {
						role = Primary
					}
					inventory[i].Persisted = &PersistedState{Member: testMember(c, i), Role: role, PrimaryDNS: c.Members[0]}
				}
			}
			for i := range inventory {
				got, err := DecideBootstrap(&c, testMember(c, i), inventory, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got.Action == SeedPrimary || got.Action == SeedReplica {
					t.Fatalf("partial inventory %d reseeded member %d", mask, i)
				}
			}
		})
	}
}

func TestInventoryContradictionsCannotSupplyDiscoveryAuthority(t *testing.T) {
	for _, name := range []string{"evidence from lost state", "higher inventory epoch", "prior primary epoch mismatch", "replacement named primary"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			c.Phase = Initialized
			inventory := retainedInventory(c, 2)
			e := testEvidence(c, 2)
			local := testMember(c, 0)
			switch name {
			case "evidence from lost state":
				inventory[0] = VolumeState{Empty: true}
			case "higher inventory epoch":
				inventory[2].Persisted.SentinelEpoch = 5
			case "prior primary epoch mismatch":
				local = testMember(c, 2)
				inventory[2].Persisted.SentinelEpoch = 3
			case "replacement named primary":
				local = testMember(c, 2)
				inventory[2] = VolumeState{Empty: true}
			}
			got, err := DecideBootstrap(&c, local, inventory, e)
			if err == nil && got.Action != Wait {
				t.Fatalf("unsafe %+v", got)
			}
		})
	}
}

func TestEvidenceCannotElectOrIgnoreHigherEpoch(t *testing.T) {
	for _, name := range []string{"valid", "duplicate", "minority", "higher minority", "same epoch conflict", "foreign cluster", "foreign member", "unpersisted", "invalid primary"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			e := testEvidence(c, 2)
			switch name {
			case "duplicate":
				e[1] = e[0]
			case "minority":
				e = e[:1]
			case "higher minority":
				e = append(e, SentinelEvidence{Member: testMember(c, 2), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[1], Epoch: 5})
			case "same epoch conflict":
				e = append(e, SentinelEvidence{Member: testMember(c, 2), ClusterID: c.ClusterID, Persisted: true, PrimaryDNS: c.Members[1], Epoch: 4})
			case "foreign cluster":
				e[1].ClusterID = "other"
			case "foreign member":
				e[1].Member.DNS = "foreign.ns"
			case "unpersisted":
				e[1].Persisted = false
			case "invalid primary":
				e[1].PrimaryDNS = "old-ip"
			}
			_, err := SelectEvidence(c, e)
			if (err == nil) != (name == "valid") {
				t.Fatalf("SelectEvidence=%v", err)
			}
		})
	}
}
