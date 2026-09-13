package redisbootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStrictStateParsing(t *testing.T) {
	c := testCluster()
	good, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseClusterState(good); err != nil || got != c {
		t.Fatalf("roundtrip=%+v,%v", got, err)
	}
	for _, input := range []string{
		`{"clusterID":"cluster-a","members":["a","b"],"phase":"Pending"}`,
		`{"clusterID":"cluster-a","members":["a","b","c","d"],"phase":"Pending"}`,
		`{"clusterID":"cluster-a","members":["a","b","c"],"phase":"Pending","extra":true}`,
		`{"clusterID":"cluster-a","clusterID":"other","members":["a","b","c"],"phase":"Pending"}`,
		string(good) + ` {}`, `null`, `[]`, string(good) + "\x00",
	} {
		if _, err := ParseClusterState([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	v := testIdentity(c, 0)
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseVolumeIdentity(data, c); err != nil || got != v {
		t.Fatalf("identity roundtrip=%+v,%v", got, err)
	}
	for _, input := range []string{`null`, string(data) + `{}`, `{"clusterID":"cluster-a","markerID":"0123456789abcdef0123456789abcdef","member":{"dns":"a","dns":"b","ordinal":0},"initialConfig":"Configured"}`} {
		if _, err := ParseVolumeIdentity([]byte(input), c); err == nil {
			t.Fatalf("accepted identity %s", input)
		}
	}
	nullOrdinal := `{"clusterID":"cluster-a","markerID":"0123456789abcdef0123456789abcdef","member":{"dns":"redis-0.redis.ns.svc","ordinal":null},"initialConfig":"Configured"}`
	if _, err := ParseVolumeIdentity([]byte(nullOrdinal), c); err == nil {
		t.Fatal("null ordinal accepted as zero")
	}
	invalidUTF8 := append(append([]byte(nil), good[:len(good)-1]...), 0xff, '}')
	if _, err := ParseClusterState(invalidUTF8); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestNewIdentityAndAtomicImmutableCreation(t *testing.T) {
	c := testCluster()
	v, err := NewVolumeIdentity(c, testMember(c, 0))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewVolumeIdentity(c, testMember(c, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v.MarkerID == other.MarkerID || v.InitialConfig != Reserved {
		t.Fatal("identity markers not independently generated")
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if WriteVolumeIdentity(context.Background(), path, v, c) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("exclusive marker creation successes=%d", successes.Load())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseVolumeIdentity(data, c)
	if err != nil || got != v {
		t.Fatalf("atomic file=%+v,%v", got, err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%o", stat.Mode().Perm())
	}
	if err := WriteVolumeIdentity(context.Background(), path, other, c); err == nil {
		t.Fatal("overwrote immutable marker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteVolumeIdentity(ctx, filepath.Join(filepath.Dir(path), "cancelled.json"), v, c); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err := WriteVolumeIdentity(context.Background(), filepath.Join(filepath.Dir(path), "missing", "identity.json"), v, c); err == nil {
		t.Fatal("unexpected directory creation")
	}
}
