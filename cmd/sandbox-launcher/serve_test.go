package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

func TestProtectedConfigurationV2Enrollment(tst *testing.T) {
	c := protectedConfig{Version: 2, Binding: configBinding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "11111111-1111-4111-8111-111111111111"}, Roots: []ed25519.PublicKey{ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("a", 64), ClockSocket: t.ClockSocket, ClockPublicKey: ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey), ClockAudience: "launcher", MaxActive: 4, Enrollment: configEnrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	wire, err := json.Marshal(c)
	if err != nil {
		tst.Fatal(err)
	}
	if _, err = decodeProtectedConfig(wire); err != nil {
		tst.Fatalf("valid v2: %v", err)
	}
	for name, mutate := range map[string]func(*protectedConfig){
		"legacy":             func(c *protectedConfig) { c.Version = 1 },
		"missing enrollment": func(c *protectedConfig) { c.Enrollment = configEnrollment{} },
		"zero node":          func(c *protectedConfig) { c.Enrollment.NodeUID = "00000000-0000-0000-0000-000000000000" },
		"noncanonical node":  func(c *protectedConfig) { c.Enrollment.NodeUID = "22222222222242228222222222222222" },
		"missing boot":       func(c *protectedConfig) { c.Enrollment.NodeBootID = "" },
		"missing audience":   func(c *protectedConfig) { c.ClockAudience = "" },
		"missing contract":   func(c *protectedConfig) { c.ContractDigest = "" },
		"bad contract":       func(c *protectedConfig) { c.ContractDigest = strings.Repeat("Z", 64) },
	} {
		tst.Run(name, func(tst *testing.T) {
			bad := c
			mutate(&bad)
			wire, _ := json.Marshal(bad)
			if _, err := decodeProtectedConfig(wire); err == nil {
				tst.Fatal("invalid config accepted")
			}
		})
	}
	enrollment, _ := json.Marshal(c.Enrollment)
	for _, bad := range []string{
		strings.Replace(string(wire), `,"enrollment":`+string(enrollment), "", 1),
		strings.Replace(string(wire), `"enrollment":`+string(enrollment), `"enrollment":null`, 1),
		strings.Replace(string(wire), `"enrollment":{`, `"enrollment":null,"enrollment":{`, 1),
		strings.Replace(string(wire), `"node_uid":`, `"unknown":true,"node_uid":`, 1),
		string(wire) + "\n", strings.Repeat(" ", 16385),
	} {
		if _, err := decodeProtectedConfig([]byte(bad)); err == nil {
			tst.Fatal("noncanonical config accepted")
		}
	}
}

func TestProtectedConfigurationRefusesUnknownDuplicateAndMissing(t *testing.T) {
	for _, wire := range []string{`{}`, `{"version":1,"version":1}`, `{"version":1,"unknown":true}`, `null`} {
		if _, err := decodeProtectedConfig([]byte(wire)); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{}`), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedConfig(file); err == nil {
		t.Fatal("unprotected config read")
	}
	link := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedConfig(link); err == nil {
		t.Fatal("symlink config read")
	}
}
func TestFixedModesRejectArgumentsAndEnvironmentSelection(t *testing.T) {
	for _, args := range [][]string{{}, {"serve", "/tmp/config"}, {"bridge", "/tmp/socket"}, {"shell"}} {
		if err := run(args); err == nil {
			t.Fatal("unfixed mode accepted")
		}
	}
	t.Setenv("SANDBOX_CONTROL_SOCKET", "/tmp/untrusted.sock")
	if err := run([]string{"bridge"}); err == nil {
		t.Fatal("user environment selected bridge")
	}
}
