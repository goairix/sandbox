package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

const bridgeHeaderLimit = 64 << 10

// bridgeHeaderConn bounds the entire first HTTP header before the HTTP parser
// sees it. Its private reader preserves bytes read beyond the header for upgrade.
type bridgeHeaderConn struct {
	net.Conn
	reader *bufio.Reader
	header *bytes.Reader
	parsed bool
}

func (c *EnrolledBridgeClient) upgrade(ctx context.Context, op *bridgeOperation, target EnrolledBridgeTarget) (net.Conn, error) {
	u := c.config.origin
	u.Scheme = "ws"
	if c.config.origin.Scheme == "https" {
		u.Scheme = "wss"
	}
	u.Path, u.RawPath = "/api/v1/namespaces/"+c.namespace+"/pods/"+target.PodName+"/exec", ""
	u.RawQuery = url.Values{"container": {"sandbox"}, "command": {"/usr/local/bin/sandbox-launcher", "bridge"}, "stdin": {"true"}, "stdout": {"true"}, "stderr": {"true"}, "tty": {"false"}}.Encode()
	// Both hooks are private and use our fixed origin, not the callback address.
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dialAPI(ctx, op) }
	dialer := websocket.Dialer{NetDialContext: dial, NetDialTLSContext: dial, Subprotocols: []string{"v5.channel.k8s.io"}, ReadBufferSize: 4096, WriteBufferSize: 32769}
	request := &http.Request{Header: make(http.Header)}
	c.config.authorize(request)
	ws, response, err := dialer.DialContext(ctx, u.String(), request.Header)
	if err != nil {
		return nil, err
	}
	if len(response.Header.Values("Sec-Websocket-Protocol")) != 1 || response.Header.Get("Sec-Websocket-Protocol") != "v5.channel.k8s.io" || len(response.Header.Values("Upgrade")) != 1 || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") || len(response.Header.Values("Connection")) != 1 || !strings.EqualFold(response.Header.Get("Connection"), "Upgrade") || response.Header.Get("Sec-Websocket-Extensions") != "" || response.Header.Get("Content-Encoding") != "" {
		ws.Close()
		return nil, ErrEnrolledBridge
	}
	return newBridgeConn(op, ws), nil
}

func (c *bridgeHeaderConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !c.parsed {
		header := make([]byte, 0, 4096)
		for {
			if len(header) == bridgeHeaderLimit {
				return 0, ErrEnrolledBridge
			}
			b, err := c.reader.ReadByte()
			if err != nil {
				return 0, err
			}
			header = append(header, b)
			if bytes.HasSuffix(header, []byte("\r\n\r\n")) {
				break
			}
		}
		c.header, c.parsed = bytes.NewReader(header), true
	}
	if c.header.Len() != 0 {
		return c.header.Read(p)
	}
	return c.reader.Read(p)
}

func (c *EnrolledBridgeClient) dialAPI(ctx context.Context, op *bridgeOperation) (net.Conn, error) {
	u := &c.config.origin
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	if err := op.own(raw); err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			raw.Close()
			return nil, err
		}
	}
	var conn net.Conn = raw
	if u.Scheme == "https" {
		config := c.config.tls.Clone()
		if config.ServerName == "" {
			config.ServerName = u.Hostname()
		}
		tlsConn := tls.Client(raw, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		conn = tlsConn
	}
	return &bridgeHeaderConn{Conn: conn, reader: bufio.NewReader(conn)}, nil
}

// get uses a fresh synchronous, owned connection; no implicit retries, proxy,
// keepalive pool, transport goroutines, redirects, or compression are involved.
func (c *EnrolledBridgeClient) get(ctx context.Context, op *bridgeOperation, path string, out any) error {
	conn, err := c.dialAPI(ctx, op)
	if err != nil {
		return err
	}
	defer conn.Close()
	u := c.config.origin
	u.Path, u.RawPath = path, ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	c.config.authorize(req)
	if err := req.Write(conn); err != nil {
		return err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	// Close the socket before body.Close: it must never drain an untrusted body.
	defer func() { conn.Close(); response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Upgrade") != "" || response.ContentLength > taskCleanupBodyLimit {
		return ErrEnrolledBridge
	}
	wire, err := io.ReadAll(io.LimitReader(response.Body, taskCleanupBodyLimit+1))
	if err != nil {
		return err
	}
	if len(wire) > taskCleanupBodyLimit || json.Unmarshal(wire, out) != nil {
		return ErrEnrolledBridge
	}
	return nil
}
