package controlrunner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiagnosticReadsFailClosedAtByteLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample")
	if err := os.WriteFile(path, make([]byte, 16385), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDiagnosticFile(path); err == nil {
		t.Fatal("oversized diagnostic file accepted")
	}
	if err := os.WriteFile(path, []byte("Threads:\t4\nVmRSS:\t321 kB\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := readDiagnosticFile(path)
	if err != nil || diagnosticField(b, "Threads") != "4" {
		t.Fatalf("%q %v", b, err)
	}
}
