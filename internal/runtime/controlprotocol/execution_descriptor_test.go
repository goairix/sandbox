package controlprotocol_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func descriptorRequest() controlprotocol.ExecutionRequest {
	return controlprotocol.ExecutionRequest{
		Argv: []string{"/bin/tool", "", "你好"}, Env: map[string]string{"Z": "last", "A": "first"},
		UID: 1000, GID: 1001, WorkDir: "/workspace", TimeoutSeconds: 30,
		Stdin: []byte{0, 0xff, '\n', 0x80}, TTY: true, RequiresNetwork: true,
	}
}

func requireDescriptor(t *testing.T, request controlprotocol.ExecutionRequest) controlprotocol.ExecutionDescriptor {
	t.Helper()
	d, err := controlprotocol.NewExecutionDescriptor(request)
	if err != nil {
		t.Fatalf("NewExecutionDescriptor: %v", err)
	}
	return d
}

// A changed field order, omitted field, embedded stdin, or wrong hash domain
// must change this independent wire expectation.
func TestExecutionDescriptorCanonical(t *testing.T) {
	r := descriptorRequest()
	d := requireDescriptor(t, r)
	stdinSum := sha256.Sum256([]byte{0, 0xff, '\n', 0x80})
	want := fmt.Sprintf(`{"version":1,"kind":"exec","argv":["/bin/tool","","你好"],"env":[{"name":"A","value":"first"},{"name":"Z","value":"last"}],"uid":1000,"gid":1001,"work_dir":"/workspace","timeout_seconds":30,"stdin_length":4,"stdin_digest":"%x","tty":true,"requires_network":true}`, stdinSum)
	if got := string(d.Canonical()); got != want {
		t.Fatalf("canonical = %s; want %s", got, want)
	}
	descriptorSum := sha256.Sum256(append([]byte("sandbox-exec-descriptor:v1\x00"), []byte(want)...))
	if got := d.Digest(); got != hex.EncodeToString(descriptorSum[:]) {
		t.Fatalf("digest = %q; want %x", got, descriptorSum)
	}
	if !bytes.Equal(d.Request().Stdin, []byte{0, 0xff, '\n', 0x80}) {
		t.Fatal("binary stdin was not preserved")
	}
	ordered := descriptorRequest()
	ordered.Env = make(map[string]string)
	ordered.Env["A"] = "first"
	ordered.Env["Z"] = "last"
	if other := requireDescriptor(t, ordered); other.Digest() != d.Digest() || !bytes.Equal(other.Canonical(), d.Canonical()) {
		t.Fatal("map insertion order changed evidence")
	}
	r.Env, r.Stdin = nil, nil
	nilDescriptor := requireDescriptor(t, r)
	r.Env, r.Stdin = map[string]string{}, []byte{}
	emptyDescriptor := requireDescriptor(t, r)
	if nilDescriptor.Digest() != emptyDescriptor.Digest() || !bytes.Equal(nilDescriptor.Canonical(), emptyDescriptor.Canonical()) {
		t.Fatal("nil and empty env/stdin differ")
	}
	if !bytes.Contains(nilDescriptor.Canonical(), []byte(`"env":[]`)) || !bytes.Contains(nilDescriptor.Canonical(), []byte(`"stdin_length":0,"stdin_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`)) {
		t.Fatal("empty payload canonical representation is incorrect")
	}
	t.Logf("canonical sample: %d bytes, %s", len(d.Canonical()), d.Canonical())
}

// Omitting any actual execution field from the digest breaks one of these cases.
func TestExecutionDescriptorBindsPayload(t *testing.T) {
	base := requireDescriptor(t, descriptorRequest())
	changes := map[string]func(*controlprotocol.ExecutionRequest){
		"argv":          func(r *controlprotocol.ExecutionRequest) { r.Argv[1] = "changed" },
		"argv_order":    func(r *controlprotocol.ExecutionRequest) { r.Argv[1], r.Argv[2] = r.Argv[2], r.Argv[1] },
		"env_value":     func(r *controlprotocol.ExecutionRequest) { r.Env["A"] = "changed" },
		"env_key":       func(r *controlprotocol.ExecutionRequest) { delete(r.Env, "A"); r.Env["B"] = "first" },
		"uid":           func(r *controlprotocol.ExecutionRequest) { r.UID++ },
		"gid":           func(r *controlprotocol.ExecutionRequest) { r.GID++ },
		"work_dir":      func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/other" },
		"timeout":       func(r *controlprotocol.ExecutionRequest) { r.TimeoutSeconds++ },
		"stdin_content": func(r *controlprotocol.ExecutionRequest) { r.Stdin[1] = 0xfe },
		"stdin_length":  func(r *controlprotocol.ExecutionRequest) { r.Stdin = append(r.Stdin, 0) },
		"tty":           func(r *controlprotocol.ExecutionRequest) { r.TTY = false },
		"network":       func(r *controlprotocol.ExecutionRequest) { r.RequiresNetwork = false },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r := descriptorRequest()
			change(&r)
			if requireDescriptor(t, r).Digest() == base.Digest() {
				t.Fatal("changed execution payload retained digest")
			}
		})
	}
}

// Input or getter aliasing must not let a caller rewrite immutable evidence.
func TestExecutionDescriptorCopies(t *testing.T) {
	r := descriptorRequest()
	d := requireDescriptor(t, r)
	wantRequest, wantCanonical, wantDigest := descriptorRequest(), d.Canonical(), d.Digest()
	r.Argv[0], r.Env["A"], r.Stdin[0] = "mutated", "mutated", 42
	mutate := func() {
		out := d.Request()
		out.Argv[0], out.Env["A"], out.Stdin[0] = "getter mutation", "getter mutation", 43
		canonical := d.Canonical()
		canonical[0] = '!'
	}
	mutate()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				mutate()
				if d.Digest() != wantDigest || !reflect.DeepEqual(d.Request(), wantRequest) || !bytes.Equal(d.Canonical(), wantCanonical) {
					t.Error("concurrent getter changed immutable evidence")
					return
				}
			}
		}()
	}
	wg.Wait()
	if d.Digest() != wantDigest || !reflect.DeepEqual(d.Request(), wantRequest) || !bytes.Equal(d.Canonical(), wantCanonical) {
		t.Fatal("source or getter mutation changed immutable evidence")
	}
}

func TestExecutionDescriptorRejects(t *testing.T) {
	cases := map[string]func(*controlprotocol.ExecutionRequest){
		"zero":         func(r *controlprotocol.ExecutionRequest) { *r = controlprotocol.ExecutionRequest{} },
		"no_args":      func(r *controlprotocol.ExecutionRequest) { r.Argv = nil },
		"empty_args":   func(r *controlprotocol.ExecutionRequest) { r.Argv = []string{} },
		"many_args":    func(r *controlprotocol.ExecutionRequest) { r.Argv = make([]string, 257); r.Argv[0] = "x" },
		"empty_argv0":  func(r *controlprotocol.ExecutionRequest) { r.Argv[0] = "" },
		"oversize_arg": func(r *controlprotocol.ExecutionRequest) { r.Argv[1] = strings.Repeat("a", 65537) },
		"nul_arg":      func(r *controlprotocol.ExecutionRequest) { r.Argv[1] = "a\x00b" },
		"utf8_arg":     func(r *controlprotocol.ExecutionRequest) { r.Argv[1] = string([]byte{0xff}) },
		"many_env": func(r *controlprotocol.ExecutionRequest) {
			r.Env = map[string]string{}
			for i := 0; i < 257; i++ {
				r.Env[fmt.Sprintf("E%d", i)] = ""
			}
		},
		"empty_env_key":       func(r *controlprotocol.ExecutionRequest) { r.Env[""] = "" },
		"oversize_env_key":    func(r *controlprotocol.ExecutionRequest) { r.Env[strings.Repeat("A", 257)] = "" },
		"numeric_env_key":     func(r *controlprotocol.ExecutionRequest) { r.Env["1A"] = "" },
		"punctuation_env_key": func(r *controlprotocol.ExecutionRequest) { r.Env["A-B"] = "" },
		"unicode_env_key":     func(r *controlprotocol.ExecutionRequest) { r.Env["中文"] = "" },
		"nul_env_key":         func(r *controlprotocol.ExecutionRequest) { r.Env["A\x00"] = "" },
		"utf8_env_key":        func(r *controlprotocol.ExecutionRequest) { r.Env[string([]byte{0xff})] = "" },
		"oversize_env_value":  func(r *controlprotocol.ExecutionRequest) { r.Env["A"] = strings.Repeat("a", 65537) },
		"nul_env_value":       func(r *controlprotocol.ExecutionRequest) { r.Env["A"] = "\x00" },
		"utf8_env_value":      func(r *controlprotocol.ExecutionRequest) { r.Env["A"] = string([]byte{0xff}) },
		"zero_uid":            func(r *controlprotocol.ExecutionRequest) { r.UID = 0 },
		"oversize_uid":        func(r *controlprotocol.ExecutionRequest) { r.UID = 2147483648 },
		"zero_gid":            func(r *controlprotocol.ExecutionRequest) { r.GID = 0 },
		"oversize_gid":        func(r *controlprotocol.ExecutionRequest) { r.GID = 2147483648 },
		"empty_work_dir":      func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "" },
		"relative_work_dir":   func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "workspace" },
		"dot_work_dir":        func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/a/./b" },
		"parent_work_dir":     func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/a/../b" },
		"slash_work_dir":      func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/a//b" },
		"trailing_work_dir":   func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/a/" },
		"oversize_work_dir":   func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/" + strings.Repeat("a", 4096) },
		"nul_work_dir":        func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/a\x00" },
		"utf8_work_dir":       func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/" + string([]byte{0xff}) },
		"zero_timeout":        func(r *controlprotocol.ExecutionRequest) { r.TimeoutSeconds = 0 },
		"oversize_timeout":    func(r *controlprotocol.ExecutionRequest) { r.TimeoutSeconds = 3601 },
		"oversize_stdin":      func(r *controlprotocol.ExecutionRequest) { r.Stdin = make([]byte, 1048577) },
		"oversize_metadata":   func(r *controlprotocol.ExecutionRequest) { *r = metadataBoundaryRequest(t, 65537) },
		"escaped_metadata":    func(r *controlprotocol.ExecutionRequest) { r.Argv[1] = strings.Repeat("\n", 32768) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := descriptorRequest()
			change(&r)
			d, err := controlprotocol.NewExecutionDescriptor(r)
			if err == nil {
				t.Fatal("invalid request accepted")
			}
			if d.Digest() != "" || len(d.Canonical()) != 0 || !reflect.DeepEqual(d.Request(), controlprotocol.ExecutionRequest{}) {
				t.Fatal("error exposed nonzero evidence")
			}
		})
	}
}

// Size is derived from actual fixed JSON bytes, not an approximate fixture.
func metadataBoundaryRequest(t *testing.T, size int) controlprotocol.ExecutionRequest {
	t.Helper()
	wire := `{"version":1,"kind":"exec","argv":["x",""],"env":[],"uid":1,"gid":1,"work_dir":"/","timeout_seconds":1,"stdin_length":0,"stdin_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","tty":false,"requires_network":false}`
	fill := strings.Repeat("a", size-len(wire))
	expected := strings.Replace(wire, `["x",""]`, `["x","`+fill+`"]`, 1)
	if len(expected) != size {
		t.Fatalf("metadata fixture has %d bytes, want %d", len(expected), size)
	}
	return controlprotocol.ExecutionRequest{Argv: []string{"x", fill}, UID: 1, GID: 1, WorkDir: "/", TimeoutSeconds: 1}
}

func TestExecutionDescriptorAcceptsBoundaries(t *testing.T) {
	cases := map[string]func(*controlprotocol.ExecutionRequest){
		"max_args": func(r *controlprotocol.ExecutionRequest) { r.Argv = make([]string, 256); r.Argv[0] = "x" },
		"max_env": func(r *controlprotocol.ExecutionRequest) {
			r.Env = map[string]string{}
			for i := 0; i < 256; i++ {
				r.Env[fmt.Sprintf("E%d", i)] = ""
			}
		},
		"max_env_key":     func(r *controlprotocol.ExecutionRequest) { r.Env = map[string]string{strings.Repeat("A", 256): ""} },
		"env_key_grammar": func(r *controlprotocol.ExecutionRequest) { r.Env = map[string]string{"_": "", "aZ_09": "", "A": ""} },
		"min_ids_timeout": func(r *controlprotocol.ExecutionRequest) { r.UID, r.GID, r.TimeoutSeconds = 1, 1, 1 },
		"max_ids_timeout": func(r *controlprotocol.ExecutionRequest) {
			r.UID, r.GID, r.TimeoutSeconds = 2147483647, 2147483647, 3600
		},
		"root":           func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/" },
		"max_work_dir":   func(r *controlprotocol.ExecutionRequest) { r.WorkDir = "/" + strings.Repeat("a", 4095) },
		"max_stdin":      func(r *controlprotocol.ExecutionRequest) { r.Stdin = bytes.Repeat([]byte{0, 0xff}, 524288) },
		"metadata_65535": func(r *controlprotocol.ExecutionRequest) { *r = metadataBoundaryRequest(t, 65535) },
		"metadata_65536": func(r *controlprotocol.ExecutionRequest) { *r = metadataBoundaryRequest(t, 65536) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := descriptorRequest()
			change(&r)
			d := requireDescriptor(t, r)
			if !reflect.DeepEqual(d.Request(), r) {
				t.Fatal("constructor changed actual execution values")
			}
			if strings.HasPrefix(name, "metadata_") {
				var size int
				if _, err := fmt.Sscanf(name, "metadata_%d", &size); err != nil {
					t.Fatal(err)
				}
				if len(d.Canonical()) != size {
					t.Fatalf("canonical length = %d, want %d", len(d.Canonical()), size)
				}
			}
		})
	}
}
