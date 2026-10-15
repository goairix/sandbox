package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

const taskCleanupBodyLimit = 262144

var errTaskCleanupRefused = errors.New("task runtime cleanup refused")

// Only static bytes enter this transport. No file loader, credential plugin,
// user callback, shared transport, environment proxy or mutable TLS container
// participates in an operation.
func (c *TaskRuntimeCleanupClient) configure(config *rest.Config) error {
	snapshot, err := snapshotStaticAPI(config)
	if err != nil {
		return err
	}
	tlsConfig := snapshot.tls
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, done, err := c.trackDial(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
		return (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, network, address)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: false, TLSClientConfig: tlsConfig, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, DialContext: dial, MaxResponseHeaderBytes: taskCleanupBodyLimit, ResponseHeaderTimeout: c.timeout, TLSHandshakeTimeout: c.timeout}
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, done, err := c.trackDial(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
		conn, err := (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		owned := tlsConfig.Clone()
		if owned.ServerName == "" {
			owned.ServerName, _, err = net.SplitHostPort(address)
			if err != nil {
				conn.Close()
				return nil, err
			}
		}
		tlsConn := tls.Client(conn, owned)
		handshakeCtx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	ownedHTTP := &http.Client{Transport: &taskCleanupTransport{base: transport, token: snapshot.token, username: snapshot.username, password: snapshot.password}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	api, err := rest.RESTClientForConfigAndClient(&rest.Config{Host: config.Host, QPS: -1, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json", GroupVersion: &schema.GroupVersion{Version: "v1"}, NegotiatedSerializer: scheme.Codecs.WithoutConversion()}}, ownedHTTP)
	if err != nil {
		return err
	}
	c.api, c.transport = api, transport
	return nil
}

type taskCleanupTransport struct {
	base                      http.RoundTripper
	token, username, password string
}

func (t *taskCleanupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.ContentLength > taskCleanupBodyLimit {
		return nil, fmt.Errorf("%w: request body bound", errTaskCleanupRefused)
	}
	request = request.Clone(request.Context())
	request.GetBody = nil
	request.Close = true
	if t.token != "" {
		request.Header.Set("Authorization", "Bearer "+t.token)
	} else if t.username != "" || t.password != "" {
		request.SetBasicAuth(t.username, t.password)
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > taskCleanupBodyLimit {
		response.Body.Close()
		return nil, fmt.Errorf("%w: response body bound", errTaskCleanupRefused)
	}
	// DoRaw overwrites body read errors for non-2xx responses. Enforce the
	// bound before SDK error conversion, joining the original Read and Close.
	body, readErr := io.ReadAll(io.LimitReader(response.Body, taskCleanupBodyLimit+1))
	closeErr := response.Body.Close()
	if len(body) > taskCleanupBodyLimit {
		return nil, fmt.Errorf("%w: response body bound", errTaskCleanupRefused)
	}
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
