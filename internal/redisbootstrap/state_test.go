package redisbootstrap

import "testing"

func testCluster() ClusterState {
	return ClusterState{ClusterID: "cluster-a", Members: [3]string{"redis-0.redis.ns.svc", "redis-1.redis.ns.svc", "redis-2.redis.ns.svc"}, Phase: Pending}
}

func testMember(c ClusterState, n int) Member { return Member{DNS: c.Members[n], Ordinal: n} }

func testIdentity(c ClusterState, n int) VolumeIdentity {
	return VolumeIdentity{ClusterID: c.ClusterID, MarkerID: "0123456789abcdef0123456789abcdef", Member: testMember(c, n), InitialConfig: Configured}
}

func TestClusterValidation(t *testing.T) {
	for _, name := range []string{"valid", "duplicate", "missing member", "ip", "uppercase", "control", "phase", "cluster id"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			switch name {
			case "duplicate":
				c.Members[1] = c.Members[0]
			case "missing member":
				c.Members[2] = ""
			case "ip":
				c.Members[0] = "127.0.0.1"
			case "uppercase":
				c.Members[0] = "Redis.ns"
			case "control":
				c.Members[0] = "redis\nns"
			case "phase":
				c.Phase = "bad"
			case "cluster id":
				c.ClusterID = ""
			}
			if err := c.Validate(); (err == nil) != (name == "valid") {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
}

func TestIdentityValidation(t *testing.T) {
	for _, name := range []string{"valid", "cluster", "marker", "ordinal", "dns", "state"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			v := testIdentity(c, 0)
			switch name {
			case "cluster":
				v.ClusterID = "other"
			case "marker":
				v.MarkerID = ""
			case "ordinal":
				v.Member.Ordinal = 3
			case "dns":
				v.Member.DNS = c.Members[1]
			case "state":
				v.InitialConfig = "bad"
			}
			if err := v.Validate(c); (err == nil) != (name == "valid") {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
}
