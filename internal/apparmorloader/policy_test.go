package apparmorloader

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const testTemplate = "profile __SANDBOX_PROFILE_NAME__ flags=(attach_disconnected,mediate_deleted) {\n  /dev/fuse rw,\n}\n"

func testPolicy() (string, string, string) {
	sum := sha256.Sum256([]byte(testTemplate))
	digest := hex.EncodeToString(sum[:])
	name := "sandbox-fuse-" + digest
	return strings.ReplaceAll(testTemplate, ProfilePlaceholder, name), name, digest
}

func TestValidatePolicy(t *testing.T) {
	rendered, name, digest := testPolicy()
	for _, tt := range []struct {
		name, policy, profile, digest string
		wantErr                       bool
	}{
		{"valid", rendered, name, digest, false},
		{"CRLF canonical", strings.ReplaceAll(rendered, "\n", "\r\n"), name, digest, false},
		{"trim canonical", " \n" + rendered + "\n ", name, digest, false},
		{"changed content", strings.ReplaceAll(rendered, " rw,", " r,"), name, digest, true},
		{"name mismatch", rendered, name + "x", digest, true},
		{"digest mismatch", rendered, name, strings.Repeat("0", 64), true},
		{"include", strings.ReplaceAll(rendered, "  /dev", "  #include <abstractions/base>\n  /dev"), name, digest, true},
		{"plain include", strings.ReplaceAll(rendered, "  /dev", "  include if exists <base>\n  /dev"), name, digest, true},
		{"extra profile", rendered + "profile other {\n}\n", name, digest, true},
		{"nested profile", strings.ReplaceAll(rendered, "  /dev", "  profile child { /tmp r, }\n  /dev"), name, digest, true},
		{"hat", strings.ReplaceAll(rendered, "  /dev", "  ^child { /tmp r, }\n  /dev"), name, digest, true},
		{"complain", strings.ReplaceAll(rendered, "attach_disconnected", "complain"), name, digest, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePolicy([]byte(tt.policy), tt.profile, tt.digest); (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePolicyRejectsForbiddenRulesWithMatchingDigest(t *testing.T) {
	for _, rule := range []string{"#include <abstractions/base>", "include if exists <base>", "profile child { /tmp r, }", "^hat { /tmp r, }", "/child{/tmp r,}", "change_profile -> child,"} {
		t.Run(rule, func(t *testing.T) {
			template := strings.Replace(testTemplate, "  /dev/fuse rw,", "  "+rule, 1)
			sum := sha256.Sum256([]byte(template))
			digest := hex.EncodeToString(sum[:])
			name := "sandbox-fuse-" + digest
			rendered := strings.Replace(template, ProfilePlaceholder, name, 1)
			if err := ValidatePolicy([]byte(rendered), name, digest); err == nil {
				t.Fatal("forbidden policy accepted despite matching identity")
			}
		})
	}
}

func TestValidatePolicyQuotedHashCannotHideRules(t *testing.T) {
	for _, tt := range []struct {
		name, rule string
		wantErr    bool
	}{
		{"quoted hash hides nested profile", `"/tmp#x" r, profile child { /tmp r, }`, true},
		{"escaped quote and hash hides nested profile", `"/tmp\"#x" r, profile child { /tmp r, }`, true},
		{"quoted hash path", `"/tmp#x" r,`, false},
		{"escaped quote and hash path", `"/tmp\"#x" r, # ordinary comment`, false},
		{"escaped backslash before close quote", `"/tmp\\" r, # ordinary comment`, false},
		{"comment with unmatched quote", `/tmp r, # "profile child {`, false},
		{"quoted keyword path", `"/tmp/profile child {}" r,`, false},
		{"unterminated quote", `"/tmp#x r,`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			template := strings.Replace(testTemplate, "  /dev/fuse rw,", "  "+tt.rule, 1)
			sum := sha256.Sum256([]byte(template))
			digest := hex.EncodeToString(sum[:])
			name := "sandbox-fuse-" + digest
			rendered := strings.Replace(template, ProfilePlaceholder, name, 1)
			if err := ValidatePolicy([]byte(rendered), name, digest); (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePolicyInheritanceOnlyExecution(t *testing.T) {
	for _, mode := range []string{"ix", "rix", "mixr", "ux", "Ux", "px", "Px", "cx", "Cx", "pix", "Pix", "cix", "Cix", "pux", "PUx", "cux", "CUx", "x"} {
		for _, leading := range []bool{false, true} {
			t.Run(mode+" leading="+map[bool]string{false: "false", true: "true"}[leading], func(t *testing.T) {
				rule := `"/bin/sh#test" ` + mode + `,`
				if leading {
					rule = mode + ` "/bin/sh#test",`
				}
				template := strings.Replace(testTemplate, "  /dev/fuse rw,", "  "+rule, 1)
				sum := sha256.Sum256([]byte(template))
				digest := hex.EncodeToString(sum[:])
				name := "sandbox-fuse-" + digest
				rendered := strings.Replace(template, ProfilePlaceholder, name, 1)
				wantErr := mode != "ix" && mode != "rix" && mode != "mixr"
				if err := ValidatePolicy([]byte(rendered), name, digest); (err != nil) != wantErr {
					t.Fatalf("err=%v, wantErr=%v", err, wantErr)
				}
			})
		}
	}
}

func TestValidatePolicyExecutionSyntaxInsidePathsAndComments(t *testing.T) {
	template := strings.Replace(testTemplate, "  /dev/fuse rw,", "  /tmp/ux r,\n  \"/tmp/Px ux,\" r, # /bin/sh Ux,\n  mount options=(rw,bind,exec) /workspace/ -> /workspace/,\n  network unix stream,\n  /bin/sh ix,", 1)
	sum := sha256.Sum256([]byte(template))
	digest := hex.EncodeToString(sum[:])
	name := "sandbox-fuse-" + digest
	if err := ValidatePolicy([]byte(strings.Replace(template, ProfilePlaceholder, name, 1)), name, digest); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePolicyExternalABIWithMatchingDigest(t *testing.T) {
	for _, tt := range []struct {
		name, prefix, rule, suffix string
		wantErr                    bool
	}{
		{"preamble magic path", "abi <abi/4.0>,\n", "", "", true},
		{"preamble quoted absolute path", "abi \"/etc/apparmor.d/abi/4.0\",\n", "", "", true},
		{"body magic path", "", "abi <abi/4.0>,", "", true},
		{"body quoted absolute path", "", `abi "/etc/apparmor.d/abi/4.0",`, "", true},
		{"inline after file rule", "", `/tmp r, abi <abi/4.0>,`, "", true},
		{"keyword adjacent magic path", "", "abi<abi/4.0>,", "", true},
		{"tab separated directive", "", "\tabi\t<abi/4.0>,", "", true},
		{"outside after profile", "", "", "abi <abi/4.0>,\n", true},
		{"comment in preamble", "# abi <abi/4.0>,\n", "", "", false},
		{"comment in body", "", `/tmp r, # abi "/etc/apparmor.d/abi/4.0",`, "", false},
		{"quoted directive text", "", `"/tmp/abi <abi/4.0>," r,`, "", false},
		{"escaped quote with hash and directive text", "", `"/tmp/\"#abi <abi/4.0>," r, # abi <abi/4.0>,`, "", false},
		{"ordinary pathname", "", "/tmp/abi r,", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			template := testTemplate
			if tt.rule != "" {
				template = strings.Replace(template, "  /dev/fuse rw,", "  "+tt.rule, 1)
			}
			template = tt.prefix + template + tt.suffix
			canonical := CanonicalPolicy([]byte(template))
			sum := sha256.Sum256(canonical)
			digest := hex.EncodeToString(sum[:])
			name := "sandbox-fuse-" + digest
			rendered := strings.Replace(string(canonical), ProfilePlaceholder, name, 1)
			if err := ValidatePolicy([]byte(rendered), name, digest); (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			} else if tt.wantErr && !strings.Contains(err.Error(), "ABI") {
				t.Fatalf("ABI must be rejected explicitly, got %v", err)
			}
		})
	}
}
