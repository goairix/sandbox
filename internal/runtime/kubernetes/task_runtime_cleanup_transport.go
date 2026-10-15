package kubernetes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

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
	if config == nil || config.Transport != nil || config.WrapTransport != nil || config.Dial != nil || config.Proxy != nil || config.ExecProvider != nil || config.AuthProvider != nil || config.AuthConfigPersister != nil || config.RateLimiter != nil || config.WarningHandler != nil || config.WarningHandlerWithContext != nil || config.BearerTokenFile != "" || config.CAFile != "" || config.CertFile != "" || config.KeyFile != "" || config.Insecure || config.Impersonate.UserName != "" || config.Impersonate.UID != "" || len(config.Impersonate.Groups) != 0 || len(config.Impersonate.Extra) != 0 {
		return fmt.Errorf("%w: static config required", errTaskCleanupRefused)
	}
	// Check raw lengths before parsing or cloning any caller-owned container.
	rawSize := len(config.CAData) + len(config.CertData) + len(config.KeyData)
	for _, value := range []string{config.Host, config.Username, config.Password, config.BearerToken, config.ServerName, config.UserAgent, config.APIPath, config.ContentType, config.AcceptContentTypes} {
		if len(value) > taskCleanupBodyLimit || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: config bound", errTaskCleanupRefused)
		}
		rawSize += len(value)
	}
	if rawSize > taskCleanupBodyLimit || len(config.CAData) > taskCleanupBodyLimit || len(config.CertData) > taskCleanupBodyLimit || len(config.KeyData) > taskCleanupBodyLimit || len(config.NextProtos) > 1 || len(config.NextProtos) == 1 && config.NextProtos[0] != "http/1.1" {
		return fmt.Errorf("%w: TLS bound/profile", errTaskCleanupRefused)
	}
	u, err := url.Parse(config.Host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || config.APIPath != "" || config.BearerToken != "" && (config.Username != "" || config.Password != "") {
		return fmt.Errorf("%w: API origin", errTaskCleanupRefused)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.Clone(config.ServerName), NextProtos: []string{"http/1.1"}}
	if len(config.CAData) != 0 {
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(config.CAData) {
			return fmt.Errorf("%w: static CA", errTaskCleanupRefused)
		}
	}
	if len(config.CertData) != 0 || len(config.KeyData) != 0 {
		certificate, err := tls.X509KeyPair(config.CertData, config.KeyData)
		if err != nil {
			return fmt.Errorf("%w: static certificate", errTaskCleanupRefused)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		done, err := c.trackDial()
		if err != nil {
			return nil, err
		}
		defer done()
		return (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, network, address)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: false, TLSClientConfig: tlsConfig, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, DialContext: dial, MaxResponseHeaderBytes: taskCleanupBodyLimit, ResponseHeaderTimeout: c.timeout, TLSHandshakeTimeout: c.timeout}
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		done, err := c.trackDial()
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
	ownedHTTP := &http.Client{Transport: &taskCleanupTransport{base: transport, token: strings.Clone(config.BearerToken), username: strings.Clone(config.Username), password: strings.Clone(config.Password)}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	response.Body = &taskCleanupBody{ReadCloser: response.Body, remaining: taskCleanupBodyLimit}
	return response, nil
}

type taskCleanupBody struct {
	io.ReadCloser
	remaining int
}

func (b *taskCleanupBody) Read(buffer []byte) (int, error) {
	if len(buffer) > b.remaining+1 {
		buffer = buffer[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(buffer)
	if n > b.remaining {
		return 0, fmt.Errorf("%w: response body bound", errTaskCleanupRefused)
	}
	b.remaining -= n
	return n, err
}
