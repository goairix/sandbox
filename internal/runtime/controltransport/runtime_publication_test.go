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
