package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// ObservationProvider must cooperatively honor context and read trusted PVC/INFO
// evidence locally; it must never construct identity from remote request input.
type ObservationProvider func(context.Context, ProofPurpose) (LocalObservation, error)

const (
	identityRequestLimit    = 2048
	identityResponseLimit   = 16384
	identityProviderTimeout = 2 * time.Second
	identityClientTimeout   = 3 * time.Second
)

var errIdentityTransport = errors.New("identity transport failed")

func identityJSONHeader(h http.Header) bool {
	if len(h.Values("Content-Type")) != 1 || len(h.Values("Content-Encoding")) != 0 {
		return false
	}
	media, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && media == "application/json"
}
func identityHTTPError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"identity request failed"}`)
}

// NewIdentityHandler fixes local membership/session and copies its private key.
// A provider must honor context; this handler does not launch detached work or
// claim to forcibly terminate an uncooperative PVC/INFO implementation. Native
// HTTP servers must also bound ReadHeaderTimeout, WriteTimeout and connections
// at server level; context alone cannot terminate a blocked ResponseWriter.Write.
func NewIdentityHandler(c ClusterState, m Member, session string, key ed25519.PrivateKey, provider ObservationProvider) (http.Handler, error) {
	if m.Validate(c) != nil || !lowerHex(session, 32) || !validProofPrivateKey(key) || provider == nil {
		return nil, errIdentityProof
	}
	private := append(ed25519.PrivateKey(nil), key...)
	slots := make(chan struct{}, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" || r.URL.Path != "/v1/identity" || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || !identityJSONHeader(r.Header) {
			identityHTTPError(w, http.StatusBadRequest)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			identityHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), identityProviderTimeout)
		defer cancel()
		// This bounds slow request bodies on a real net/http connection. Recorder or
		// middleware may not support deadlines; server-level read limits remain required.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(identityProviderTimeout))
		body, err := io.ReadAll(io.LimitReader(r.Body, identityRequestLimit+1))
		if err != nil || len(body) > identityRequestLimit || ctx.Err() != nil {
			identityHTTPError(w, http.StatusBadRequest)
			return
		}
		var challenge IdentityChallenge
		if strictIdentityJSON(body, &challenge) != nil || validateChallenge(challenge) != nil {
			identityHTTPError(w, http.StatusBadRequest)
			return
		}
		observation, err := provider(ctx, challenge.Purpose)
		if ctx.Err() != nil {
			identityHTTPError(w, http.StatusGatewayTimeout)
			return
		}
		if err != nil {
			identityHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		proof, err := SignIdentityProof(private, c, m, session, challenge, observation)
		if err != nil {
			identityHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		data, err := json.Marshal(proof)
		if err != nil || len(data) > identityResponseLimit {
			identityHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}), nil
}

// strictIdentityJSON enforces exact field spelling/presence recursively, rather
// than encoding/json's case-insensitive matching. Only optional pointer fields
// may be null (empty/reserved observation); required scalar/object nulls fail.
func strictIdentityJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	typ := reflect.TypeOf(destination)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return errIdentityTransport
	}
	if strictIdentityValue(decoder, typ.Elem(), false) != nil {
		return errIdentityTransport
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errIdentityTransport
	}
	if json.Unmarshal(data, destination) != nil {
		return errIdentityTransport
	}
	return nil
}

func strictIdentityValue(d *json.Decoder, typ reflect.Type, optional bool) error {
	if typ.Kind() == reflect.Pointer {
		return strictIdentityValue(d, typ.Elem(), true)
	}
	token, err := d.Token()
	if err != nil {
		return errIdentityTransport
	}
	if token == nil {
		if optional {
			return nil
		}
		return errIdentityTransport
	}
	switch typ.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return errIdentityTransport
		}
		fields := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		seen := make(map[string]bool, len(fields))
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errIdentityTransport
			}
			name, ok := key.(string)
			field, known := fields[name]
			if !ok || !known || seen[name] {
				return errIdentityTransport
			}
			seen[name] = true
			if strictIdentityValue(d, field, false) != nil {
				return errIdentityTransport
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') || len(seen) != len(fields) {
			return errIdentityTransport
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return errIdentityTransport
		}
	case reflect.Bool:
		if _, ok := token.(bool); !ok {
			return errIdentityTransport
		}
	case reflect.Int, reflect.Uint64:
		if _, ok := token.(json.Number); !ok {
			return errIdentityTransport
		}
	default:
		return errIdentityTransport
	}
	return nil
}

func newIdentityHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{Proxy: nil, DialContext: dialer.DialContext, DisableCompression: true, DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: identityResponseLimit, TLSHandshakeTimeout: time.Second, ExpectContinueTimeout: time.Second}
	return &http.Client{Transport: transport, Timeout: identityClientTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// FetchIdentityProof contacts only validated fixed member DNS on identity port
// 18080, with no environment proxy, redirects, compression, connection reuse or
// retries. It fully reads bounded response bytes and verifies the fresh proof.
// The key/expected marker must come from trusted registration; current INFO is
// independently authenticated by the caller, never by this HTTP transport.
func FetchIdentityProof(ctx context.Context, c ClusterState, m Member, key ed25519.PublicKey, ch IdentityChallenge, expected *VolumeIdentity, current *AuthenticatedEndpoint) (IdentityProof, error) {
	if m.Validate(c) != nil {
		return IdentityProof{}, errIdentityTransport
	}
	return fetchIdentityProof(ctx, newIdentityHTTPClient(), "http://"+m.DNS+":18080/v1/identity", c, m, key, ch, expected, current)
}

// fetchIdentityProof is unexported so httptest can supply a local test endpoint.
// Production callers cannot supply URLs or custom transports.
func fetchIdentityProof(ctx context.Context, client *http.Client, url string, c ClusterState, m Member, key ed25519.PublicKey, ch IdentityChallenge, expected *VolumeIdentity, current *AuthenticatedEndpoint) (IdentityProof, error) {
	if ctx == nil || client == nil || m.Validate(c) != nil || validateChallenge(ch) != nil || !validProofPublicKey(key) {
		return IdentityProof{}, errIdentityTransport
	}
	ctx, cancel := context.WithTimeout(ctx, identityClientTimeout)
	defer cancel()
	data, _ := json.Marshal(ch)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return IdentityProof{}, errIdentityTransport
	}
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	response, err := client.Do(request)
	if err != nil {
		return IdentityProof{}, errIdentityTransport
	}
	// Closing is cleanup only: a fully consumed bounded body is independently
	// checked below. A Close error cannot authenticate a rejected response or
	// invalidate verified response bytes; never expose transport diagnostics.
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || !identityJSONHeader(response.Header) {
		return IdentityProof{}, errIdentityTransport
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, identityResponseLimit+1))
	if err != nil || len(body) > identityResponseLimit || ctx.Err() != nil {
		return IdentityProof{}, errIdentityTransport
	}
	var proof IdentityProof
	if strictIdentityJSON(body, &proof) != nil || VerifyIdentityProof(key, c, m, ch, proof, expected, current) != nil {
		return IdentityProof{}, errIdentityTransport
	}
	return proof, nil
}
