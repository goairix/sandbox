package deployment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putFile(t *testing.T, path string, wire []byte) {
	t.Helper()
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedFileAndProjectedSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")
	putFile(t, path, []byte("1234"))
	if wire, err := boundedFile(path, 4, "credential"); err != nil || string(wire) != "1234" {
		t.Fatalf("boundary: %q %v", wire, err)
	}
	if _, err := boundedFile(path, 3, "credential"); err == nil {
		t.Fatal("accepted oversize")
	}
	if _, err := boundedFile(dir, 4, "credential"); err == nil {
		t.Fatal("accepted directory")
	}
	if _, err := boundedFile(path+"missing", 4, "credential"); err == nil {
		t.Fatal("accepted missing")
	}
	if err := os.Mkdir(filepath.Join(dir, "..epoch"), 0700); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(dir, "..epoch", "token"), []byte("token"))
	if err := os.Symlink("..epoch", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/token", filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	if wire, err := boundedFile(filepath.Join(dir, "token"), 5, "token"); err != nil || string(wire) != "token" {
		t.Fatalf("projection: %q %v", wire, err)
	}
}

func TestSigningPrivateKey(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	valid := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "key")
	putFile(t, path, valid)
	got, err := signingPrivateKey(path, "root key")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("PKCS8: %v", err)
	}
	for _, wire := range [][]byte{append(bytes.Clone(valid), valid...), []byte("PRIVATE SENTINEL"), bytes.Repeat([]byte("x"), 4097), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})} {
		putFile(t, path, wire)
		if _, err := signingPrivateKey(path, "root key"); err == nil || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatalf("unsafe signing key result %v", err)
		}
	}
}

func TestKubeconfigInlinesBoundedCredentialPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	putFile(t, filepath.Join(dir, "token"), []byte(" TOKEN-SENTINEL \n"))
	putFile(t, path, []byte(kubeFixture("    tokenFile: token\n")))
	cfg, err := loadKubeconfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerToken != "TOKEN-SENTINEL" || cfg.BearerTokenFile != "" || cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
		t.Fatal("credential path escaped loader")
	}
	putFile(t, filepath.Join(dir, "token"), bytes.Repeat([]byte("x"), 16385))
	if _, err := loadKubeconfig(path); err == nil {
		t.Fatal("accepted oversize token")
	}
	for _, auth := range []string{
		"    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: /must-not-execute\n      interactiveMode: Never\n",
		"    auth-provider:\n      name: oidc\n",
	} {
		putFile(t, path, []byte(kubeFixture(auth)))
		if _, err := loadKubeconfig(path); err == nil {
			t.Fatal("accepted external authentication")
		}
	}
	putFile(t, path, []byte(kubeFixture("    token: safe\n")+"- name: unused\n  user:\n    exec:\n      command: /must-not-execute\n"))
	if _, err := loadKubeconfig(path); err == nil {
		t.Fatal("accepted unused external authentication")
	}
}

func TestKubeconfigCredentialPathsAndTLSRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	for _, name := range []string{"ca", "cert", "key"} {
		putFile(t, filepath.Join(dir, name), []byte(name+"-sentinel"))
	}
	wire := strings.Replace(kubeFixture("    client-certificate: cert\n    client-key: key\n"), "    server:", "    certificate-authority: ca\n    server:", 1)
	putFile(t, path, []byte(wire))
	cfg, err := loadKubeconfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.CAData) != "ca-sentinel" || string(cfg.CertData) != "cert-sentinel" || string(cfg.KeyData) != "key-sentinel" || cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
		t.Fatal("credential files were not owned")
	}
	for _, bad := range []string{
		strings.Replace(wire, "https://", "http://", 1),
		strings.Replace(wire, "    server:", "    insecure-skip-tls-verify: true\n    server:", 1),
		strings.Replace(wire, "    server:", "    proxy-url: http://127.0.0.1:8888\n    server:", 1),
	} {
		putFile(t, path, []byte(bad))
		if _, err := loadKubeconfig(path); err == nil {
			t.Fatal("accepted nonstatic TLS profile")
		}
	}
}

func kubeFixture(auth string) string {
	return "apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: cluster\n    user: user\nclusters:\n- name: cluster\n  cluster:\n    server: https://127.0.0.1:6443\nusers:\n- name: user\n  user:\n" + auth
}
