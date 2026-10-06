package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

// Permissive JSON decoding would accept unsigned ambiguity in every nesting.
func TestPublicationRejectsMalformedWire(t *testing.T) {
	f := newFixture(t)
	cert, proof := f.wires(t)
	for name, wire := range map[string][]byte{
		"unknown envelope":        bytes.Replace(cert, []byte(`{"claims":`), []byte(`{"unknown":0,"claims":`), 1),
		"duplicate envelope":      bytes.Replace(cert, []byte(`{"claims":`), []byte(`{"signature":"AA==","claims":`), 1),
		"null envelope":           []byte(`null`),
		"null claims":             []byte(`{"claims":null,"signature":"AA=="}`),
		"missing envelope":        []byte(`{"claims":{}}`),
		"unknown nested":          bytes.Replace(cert, []byte(`"runtime":{`), []byte(`"runtime":{"unknown":0,`), 1),
		"duplicate nested":        bytes.Replace(cert, []byte(`"runtime":{`), []byte(`"runtime":{"id":"first",`), 1),
		"missing nested":          bytes.Replace(cert, []byte(`,"boot_id":"boot"`), nil, 1),
		"null nested":             bytes.Replace(cert, []byte(`"boot_id":"boot"`), []byte(`"boot_id":null`), 1),
		"case alias":              bytes.Replace(cert, []byte(`"boot_id"`), []byte(`"Boot_ID"`), 1),
		"float":                   bytes.Replace(cert, []byte(`"generation":1`), []byte(`"generation":1.0`), 1),
		"trailing":                append(append([]byte(nil), cert...), []byte(` {}`)...),
		"invalid utf8":            append(append([]byte(nil), cert...), 0xff),
		"invalid escaped unicode": bytes.Replace(cert, []byte(`"boot"`), []byte(`"\ud800"`), 1),
		"certificate too large":   append(bytes.Repeat([]byte(" "), 4097), cert...),
		"signature short":         replaceJSON(t, cert, func(m map[string]any) { m["signature"] = "AA==" }),
		"public key short":        replaceJSON(t, cert, func(m map[string]any) { m["claims"].(map[string]any)["public_key"] = "AA==" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.verifier.VerifyHistoricalCertificate(wire, f.context.Certificate); err == nil {
				t.Fatal("accepted malformed certificate")
			}
		})
	}
	for name, wire := range map[string][]byte{
		"unknown ready":                bytes.Replace(proof, []byte(`"gate_state":"open"`), []byte(`"extra":0,"gate_state":"open"`), 1),
		"duplicate claim":              bytes.Replace(proof, []byte(`"claim_id":`), []byte(`"lease_id":99,"claim_id":`), 1),
		"null claim":                   bytes.Replace(proof, []byte(`"lease_id":3`), []byte(`"lease_id":null`), 1),
		"missing ready":                bytes.Replace(proof, []byte(`"gate_state":"open",`), nil, 1),
		"null certificate":             replaceJSON(t, proof, func(m map[string]any) { m["certificate"] = nil }),
		"proof too large":              append(bytes.Repeat([]byte(" "), 8193), proof...),
		"nested certificate too large": bytes.Replace(proof, []byte(`"certificate":{`), append([]byte(`"certificate":{`), bytes.Repeat([]byte(" "), 4097)...), 1),
		"signature short":              replaceJSON(t, proof, func(m map[string]any) { m["signature"] = "AA==" }),
		"trailing":                     append(append([]byte(nil), proof...), []byte(`null`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.verifier.VerifyHistorical(wire, f.context); err == nil {
				t.Fatal("accepted malformed ready proof")
			}
		})
	}
}

func replaceJSON(t *testing.T, wire []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublicationNamespaceGrammar(t *testing.T) {
	f := newFixture(t)
	for _, root := range []string{"/prefix/scope/cell/", "/multi/part/scope/cell/", "/" + strings.Repeat("a", 240) + "/scope/cell/"} {
		binding := TrustBinding{root, "authority", "target", "epoch"}
		if _, err := NewPublicationVerifier(binding, []ed25519.PublicKey{ed25519.PublicKey(f.root[32:])}); err != nil {
			t.Fatalf("valid namespace rejected: %q: %v", root, err)
		}
	}
	for _, root := range []string{"", "/", "prefix/scope/cell/", "/scope/cell/", "/prefix/scope/cell", "/prefix//scope/cell/", "/prefix/../cell/", "/prefix/空/cell/", "/prefix/" + strings.Repeat("a", 129) + "/cell/", "/" + strings.Repeat("a", 500) + "/scope/cell/"} {
		binding := TrustBinding{root, "authority", "target", "epoch"}
		if _, err := NewPublicationVerifier(binding, []ed25519.PublicKey{ed25519.PublicKey(f.root[32:])}); err == nil {
			t.Fatalf("invalid namespace accepted: %q", root)
		}
	}
}

func TestPublicationExactWireLimits(t *testing.T) {
	f := newFixture(t)
	cert, proof := f.wires(t)
	padded := append([]byte{'{'}, bytes.Repeat([]byte(" "), 4096-len(cert))...)
	padded = append(padded, cert[1:]...)
	if _, err := f.verifier.VerifyHistoricalCertificate(padded, f.context.Certificate); err != nil {
		t.Fatal("certificate at exact limit rejected:", err)
	}
	embedded := bytes.Replace(proof, cert, padded, 1)
	if _, err := f.verifier.VerifyHistorical(embedded, f.context); err != nil {
		t.Fatal("embedded certificate at exact limit rejected:", err)
	}
	complete := append(append([]byte(nil), proof...), bytes.Repeat([]byte(" "), 8192-len(proof))...)
	if _, err := f.verifier.VerifyHistorical(complete, f.context); err != nil {
		t.Fatal("proof at exact limit rejected:", err)
	}
	if _, err := f.verifier.VerifyHistorical(append(complete, ' '), f.context); err == nil {
		t.Fatal("proof over exact limit accepted")
	}
}
