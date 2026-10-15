package fuseprotocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Only the public key is installed in sandbox Pods. The signing seed remains
// in server-side state and never appears in argv, Pod metadata or a workspace.
const ProbeControlPublicKeyEnv = "SANDBOX_PROBE_CONTROL_PUBLIC_KEY"
const probeControlDomain = "sandbox/workspace-probe/control/v1"

type ProbeControlRequest struct {
	Version       int    `json:"version"`
	Command       string `json:"command"`
	RuntimeUID    string `json:"runtime_uid"`
	Generation    int64  `json:"generation"`
	Authorization string `json:"authorization"`
}

func probeControlMessage(command, uid string, generation int64) ([]byte, error) {
	if (command != "register" && command != "cleanup") || !ValidIdentity(uid) || generation <= 0 {
		return nil, fmt.Errorf("invalid workspace control identity")
	}
	return json.Marshal(struct {
		Domain     string `json:"domain"`
		Command    string `json:"command"`
		RuntimeUID string `json:"runtime_uid"`
		Generation int64  `json:"generation"`
	}{probeControlDomain, command, uid, generation})
}

func SignProbeControl(key ed25519.PrivateKey, command, uid string, generation int64) (ProbeControlRequest, error) {
	message, err := probeControlMessage(command, uid, generation)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return ProbeControlRequest{}, fmt.Errorf("workspace control signer is unavailable")
	}
	return ProbeControlRequest{Version: Version, Command: command, RuntimeUID: uid, Generation: generation,
		Authorization: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, message))}, nil
}

func VerifyProbeControl(key ed25519.PublicKey, request ProbeControlRequest, command, uid string, generation int64) bool {
	if len(key) != ed25519.PublicKeySize || request.Version != Version || request.Command != command || request.RuntimeUID != uid || request.Generation != generation {
		return false
	}
	message, err := probeControlMessage(command, uid, generation)
	if err != nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Authorization)
	return err == nil && len(signature) == ed25519.SignatureSize && ed25519.Verify(key, message, signature)
}

func ParseProbeControlPublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("workspace control verification key is unavailable")
	}
	return ed25519.PublicKey(key), nil
}
