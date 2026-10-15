package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func publicationFramePayload(wire []byte) []byte {
	frame := make([]byte, 4, 4+len(wire))
	binary.BigEndian.PutUint32(frame, uint32(len(wire)))
	return append(frame, wire...)
}

// Opaque transport bytes deliberately convey no cryptographic authority.
func publicationFrameResponse() RuntimePublicationResponse {
	return RuntimePublicationResponse{Version: 1, Purpose: RuntimeEnrolledPublicationPurpose, Enrollment: p.EnrolledRuntimeEvidence{Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: bytes.Repeat([]byte("r"), 4096), Activation: bytes.Repeat([]byte("a"), 8000), IssuerCertificate: []byte("i")}, ObserverCertificate: bytes.Repeat([]byte("o"), 2048), Receipt: bytes.Repeat([]byte("e"), 4096)}, Ready: bytes.Repeat([]byte("p"), 8192)}
}

func TestRuntimePublicationRawPreflight(t *testing.T) {
	request := RuntimePublicationEnvelope{Version: 1, Purpose: RuntimeEnrolledPublicationPurpose, PublicationCertificate: []byte("certificate"), Claim: p.ClaimReference{ClaimID: "11111111-1111-4111-8111-111111111111", CreateRevision: 1, LeaseID: 2}}
	response := publicationFrameResponse()
	// Keep duplicate-object regressions below the outer bound so only the
	// pre-decode schema/nested caps can reject them.
	response.Enrollment.Request.RuntimeCertificate = []byte("r")
	response.Enrollment.Request.Activation = []byte("a")
	response.Enrollment.ObserverCertificate = []byte("o")
	response.Enrollment.Receipt = []byte("e")
	response.Ready = []byte("p")
	requestWire, _ := json.Marshal(request)
	responseWire, _ := json.Marshal(response)
	if _, err := ReadRuntimeRequest(bytes.NewReader(publicationFramePayload(requestWire))); err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	if _, err := ReadRuntimePublicationResponse(bytes.NewReader(publicationFramePayload(responseWire))); err != nil {
		t.Fatalf("canonical response: %v", err)
	}
	for _, valid := range []struct {
		wire  []byte
		check func([]byte) error
	}{{requestWire, validateRuntimePublicationRequestRaw}, {responseWire, validateRuntimePublicationResponseRaw}} {
		if err := valid.check(valid.wire); err != nil {
			t.Fatalf("valid raw preflight: %v", err)
		}
	}
	duplicate := func(wire []byte, field string, first any) []byte {
		value, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		key := `"` + field + `":`
		return []byte(strings.Replace(string(wire), key, key+string(value)+","+key, 1))
	}
	oversizedRequest := response.Enrollment.Request
	oversizedRequest.Activation = bytes.Repeat([]byte("a"), 12300) // aggregate >16384, individual blob within cap
	oversizedEnrollment := response.Enrollment
	oversizedEnrollment.Request = oversizedRequest
	oversizedResponse := response
	oversizedResponse.Enrollment = oversizedEnrollment
	aggregateWire, _ := json.Marshal(oversizedResponse)
	cases := []struct {
		name  string
		wire  []byte
		check func([]byte) error
	}{
		{"request-certificate", duplicate(requestWire, "publication_certificate", make([]byte, 5000)), validateRuntimePublicationRequestRaw},
		{"ready", duplicate(responseWire, "ready", make([]byte, 9000)), validateRuntimePublicationResponseRaw},
		{"observer", duplicate(responseWire, "ObserverCertificate", make([]byte, 3000)), validateRuntimePublicationResponseRaw},
		{"receipt", duplicate(responseWire, "Receipt", make([]byte, 5000)), validateRuntimePublicationResponseRaw},
		{"runtime", duplicate(responseWire, "runtime_certificate", make([]byte, 5000)), validateRuntimePublicationResponseRaw},
		{"issuer", duplicate(responseWire, "issuer_certificate", make([]byte, 5000)), validateRuntimePublicationResponseRaw},
		{"activation", duplicate(responseWire, "activation", make([]byte, 17000)), validateRuntimePublicationResponseRaw},
		{"request-object", duplicate(responseWire, "Request", oversizedRequest), validateRuntimePublicationResponseRaw},
		{"enrollment-object", duplicate(responseWire, "enrollment", oversizedEnrollment), validateRuntimePublicationResponseRaw},
		{"aggregate", aggregateWire, validateRuntimePublicationResponseRaw},
		{"claim-duplicate", duplicate(requestWire, "claim_id", request.Claim.ClaimID), validateRuntimePublicationRequestRaw},
		{"case-alias", []byte(strings.Replace(string(responseWire), `"ready":`, `"Ready":"cA==","ready":`, 1)), validateRuntimePublicationResponseRaw},
		{"escaped-key", []byte(strings.Replace(string(responseWire), `"ready":`, `"r\u0065ady":`, 1)), validateRuntimePublicationResponseRaw},
		{"nested-unknown", []byte(strings.Replace(string(responseWire), `"Request":`, `"unknown":1,"Request":`, 1)), validateRuntimePublicationResponseRaw},
		{"nested-null", []byte(strings.Replace(string(responseWire), `"Request":`, `"Request":null,"Request":`, 1)), validateRuntimePublicationResponseRaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.wire) > MaxRuntimePublicationRequest {
				t.Fatal("regression unexpectedly exceeds outer cap")
			}
			// Directly assert rejection by the raw-only stage: a later canonical
			// error after typed []byte decoding cannot satisfy this assertion.
			if err := tc.check(tc.wire); err == nil {
				t.Fatal("raw preflight accepted earlier oversized duplicate")
			}
		})
	}
}

func TestRuntimePublicationFrames(t *testing.T) {
	e := RuntimePublicationEnvelope{Version: 1, Purpose: RuntimeEnrolledPublicationPurpose, PublicationCertificate: []byte("certificate"), Claim: p.ClaimReference{ClaimID: "11111111-1111-4111-8111-111111111111", CreateRevision: 1, LeaseID: 2}}
	var buffer bytes.Buffer
	if err := WriteRuntimePublicationRequest(&buffer, e); err != nil {
		t.Fatal(err)
	}
	request, err := ReadRuntimeRequest(bytes.NewReader(buffer.Bytes()))
	if err != nil || request.Publication == nil || !reflect.DeepEqual(*request.Publication, e) || request.CloseData != nil || request.Quiesce != nil || request.Archive != nil {
		t.Fatalf("publication request routed incorrectly: %+v %v", request, err)
	}
	wire, _ := json.Marshal(e)
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(wire), `"version":1`, `"version":1,"version":1`, 1)),
		[]byte(strings.Replace(string(wire), `"claim":`, `"unknown":1,"claim":`, 1)),
		[]byte(strings.Replace(string(wire), `"claim":`, `"activation":null,"claim":`, 1)),
		[]byte(strings.Replace(string(wire), `"publication_certificate":"Y2VydGlmaWNhdGU="`, `"publication_certificate":null`, 1)),
		append(bytes.Clone(wire), '\n'), []byte("null"), append(bytes.Clone(wire), bytes.Repeat([]byte(" "), MaxRuntimePublicationRequest+1-len(wire))...),
	} {
		if _, err := ReadRuntimeRequest(bytes.NewReader(publicationFramePayload(bad))); err == nil {
			t.Fatal("invalid publication request accepted")
		}
	}
	if _, err := ReadRuntimeRequest(bytes.NewReader(append(publicationFramePayload(wire), 0))); err == nil {
		t.Fatal("publication stdin/trailing byte accepted")
	}
	for _, mutate := range []func(*RuntimePublicationEnvelope){
		func(e *RuntimePublicationEnvelope) { e.PublicationCertificate = make([]byte, 4097) },
		func(e *RuntimePublicationEnvelope) { e.Claim.ClaimID = "00000000-0000-0000-0000-000000000000" },
		func(e *RuntimePublicationEnvelope) { e.Claim.CreateRevision = 0 },
		func(e *RuntimePublicationEnvelope) { e.Claim.LeaseID = -1 },
	} {
		bad := e
		mutate(&bad)
		if err := WriteRuntimePublicationRequest(io.Discard, bad); err == nil {
			t.Fatal("invalid publication attribution accepted")
		}
	}
	response := publicationFrameResponse()
	buffer.Reset()
	if err := WriteRuntimePublicationResponse(&buffer, response); err != nil {
		t.Fatal(err)
	}
	if buffer.Len() <= 32768 || buffer.Len() > 65540 {
		t.Fatalf("independent large response bound not exercised: %d", buffer.Len())
	}
	got, err := ReadRuntimePublicationResponse(bytes.NewReader(buffer.Bytes()))
	if err != nil || !reflect.DeepEqual(got, response) {
		t.Fatalf("large response: %v", err)
	}
	if _, err := ReadRuntimePublicationResponse(bytes.NewReader(append(bytes.Clone(buffer.Bytes()), 0))); err == nil {
		t.Fatal("response trailing byte accepted")
	}
	for _, mutate := range []func(*RuntimePublicationResponse){
		func(e *RuntimePublicationResponse) { e.Ready = make([]byte, 8193) },
		func(e *RuntimePublicationResponse) { e.Enrollment.ObserverCertificate = make([]byte, 2049) },
		func(e *RuntimePublicationResponse) { e.Enrollment.Receipt = make([]byte, 4097) },
		func(e *RuntimePublicationResponse) { e.Enrollment.Request.RuntimeCertificate = make([]byte, 4097) },
		func(e *RuntimePublicationResponse) { e.Enrollment.Request.IssuerCertificate = make([]byte, 4097) },
		func(e *RuntimePublicationResponse) { e.Enrollment.Request.Activation = make([]byte, 16385) },
		func(e *RuntimePublicationResponse) { e.Enrollment.Request.Activation = make([]byte, 12000) },
	} {
		bad := response
		mutate(&bad)
		if err := WriteRuntimePublicationResponse(io.Discard, bad); err == nil {
			t.Fatal("nested oversized response encoded")
		}
		wire, _ := json.Marshal(bad)
		if _, err := ReadRuntimePublicationResponse(bytes.NewReader(publicationFramePayload(wire))); err == nil {
			t.Fatal("nested oversized response decoded")
		}
	}
	wire, _ = json.Marshal(response)
	for _, bad := range [][]byte{[]byte("null"), append(bytes.Clone(wire), '\n'), []byte(strings.Replace(string(wire), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(wire), `"enrollment":`, `"success":true,"enrollment":`, 1)), bytes.Repeat([]byte("x"), 65537)} {
		if _, err := ReadRuntimePublicationResponse(bytes.NewReader(publicationFramePayload(bad))); err == nil {
			t.Fatal("noncanonical/oversized response accepted")
		}
	}
}
