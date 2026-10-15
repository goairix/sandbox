package kubernetes

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/client-go/rest"
)

// staticAPIConfig owns only immutable configuration, never requests or slots.
type staticAPIConfig struct {
	origin                    url.URL
	tls                       *tls.Config
	token, username, password string
}

func snapshotStaticAPI(config *rest.Config) (*staticAPIConfig, error) {
	if config == nil || config.Transport != nil || config.WrapTransport != nil || config.Dial != nil || config.Proxy != nil || config.ExecProvider != nil || config.AuthProvider != nil || config.AuthConfigPersister != nil || config.RateLimiter != nil || config.WarningHandler != nil || config.WarningHandlerWithContext != nil || config.BearerTokenFile != "" || config.CAFile != "" || config.CertFile != "" || config.KeyFile != "" || config.Insecure || config.Impersonate.UserName != "" || config.Impersonate.UID != "" || len(config.Impersonate.Groups) != 0 || len(config.Impersonate.Extra) != 0 {
		return nil, fmt.Errorf("%w: static config required", errTaskCleanupRefused)
	}
	rawSize := len(config.CAData) + len(config.CertData) + len(config.KeyData)
	for _, value := range []string{config.Host, config.Username, config.Password, config.BearerToken, config.ServerName, config.UserAgent, config.APIPath, config.ContentType, config.AcceptContentTypes} {
		if len(value) > taskCleanupBodyLimit || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("%w: config bound", errTaskCleanupRefused)
		}
		rawSize += len(value)
	}
	if rawSize > taskCleanupBodyLimit || len(config.CAData) > taskCleanupBodyLimit || len(config.CertData) > taskCleanupBodyLimit || len(config.KeyData) > taskCleanupBodyLimit || len(config.NextProtos) > 1 || len(config.NextProtos) == 1 && config.NextProtos[0] != "http/1.1" {
		return nil, fmt.Errorf("%w: TLS bound/profile", errTaskCleanupRefused)
	}
	u, err := url.Parse(config.Host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || config.APIPath != "" || config.BearerToken != "" && (config.Username != "" || config.Password != "") {
		return nil, fmt.Errorf("%w: API origin", errTaskCleanupRefused)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.Clone(config.ServerName), NextProtos: []string{"http/1.1"}}
	if len(config.CAData) != 0 {
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(config.CAData) {
			return nil, fmt.Errorf("%w: static CA", errTaskCleanupRefused)
		}
	}
	if len(config.CertData) != 0 || len(config.KeyData) != 0 {
		certificate, err := tls.X509KeyPair(config.CertData, config.KeyData)
		if err != nil {
			return nil, fmt.Errorf("%w: static certificate", errTaskCleanupRefused)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return &staticAPIConfig{origin: *u, tls: tlsConfig, token: strings.Clone(config.BearerToken), username: strings.Clone(config.Username), password: strings.Clone(config.Password)}, nil
}

func (s *staticAPIConfig) authorize(r *http.Request) {
	if s.token != "" {
		r.Header.Set("Authorization", "Bearer "+s.token)
	} else if s.username != "" || s.password != "" {
		r.SetBasicAuth(s.username, s.password)
	}
}
