// Package backend implements the agent side of the agent–backend protocol.
package backend

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thumbops/agent/internal/protocol"
)

// StatusError is an unsuccessful HTTP response from the backend.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("backend responded %d: %s", e.Code, e.Body)
}

// Code returns the HTTP status code of a backend error, or 0.
func Code(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// Unauthorized reports whether the backend no longer accepts the agent: a 401
// response, or a client certificate rejected during the TLS handshake
// (expired, revoked, signed by an unknown CA). In the second case no HTTP
// response arrives, because the server or reverse proxy closes first.
func Unauthorized(err error) bool {
	return Code(err) == http.StatusUnauthorized || certificateRejected(err)
}

// TLS alerts a server uses to reject the client certificate, as crypto/tls
// formats them. The alert type is not exported, so it is recognized by its
// text, inside a net.OpError with Op "remote error".
var certificateAlerts = map[string]bool{
	"tls: bad certificate":               true,
	"tls: unsupported certificate":       true,
	"tls: revoked certificate":           true,
	"tls: expired certificate":           true,
	"tls: unknown certificate":           true,
	"tls: unknown certificate authority": true,
	"tls: certificate required":          true,
}

func certificateRejected(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error" && op.Err != nil && certificateAlerts[op.Err.Error()]
}

type Options struct {
	BaseURL   string      // e.g. https://agent.thumbops.mobiletechnologies.cloud
	TLS       *tls.Config // client certificate (mTLS) and server CA; nil = default
	UserAgent string      // e.g. thumbops-agent/0.1.0
}

type Client struct {
	base      string
	userAgent string
	tlsConfig *tls.Config
	http      atomic.Pointer[http.Client]
}

func New(o Options) *Client {
	c := &Client{base: strings.TrimRight(o.BaseURL, "/"), userAgent: o.UserAgent, tlsConfig: o.TLS}
	c.http.Store(c.newHTTPClient())
	return c
}

func (c *Client) newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.tlsConfig != nil {
		tr.TLSClientConfig = c.tlsConfig
	}
	return &http.Client{Transport: tr}
}

// ResetConnections must be called when the client certificate changes (after
// registration or a renewal). The certificate is presented only during the
// TLS handshake: without this reset the client would keep reusing the
// connections opened with the previous certificate, or with none.
// In-flight requests finish on the old connection.
func (c *Client) ResetConnections() {
	old := c.http.Swap(c.newHTTPClient())
	old.CloseIdleConnections()
}

const defaultTimeout = 30 * time.Second

func (c *Client) do(ctx context.Context, method, path, bearer string, timeout time.Duration, in, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Load().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("invalid backend response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Register authenticates with the single-use bootstrap token and obtains the certificate.
func (c *Client) Register(ctx context.Context, bootstrapToken string, req protocol.RegisterRequest) (*protocol.RegisterResponse, error) {
	var out protocol.RegisterResponse
	if _, err := c.do(ctx, http.MethodPost, "/v1/register", bootstrapToken, defaultTimeout, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RenewCertificate sends a new CSR, authenticating with the still valid certificate.
func (c *Client) RenewCertificate(ctx context.Context, csr string) (*protocol.CertificateResponse, error) {
	var out protocol.CertificateResponse
	if _, err := c.do(ctx, http.MethodPost, "/v1/agent/certificate", "", defaultTimeout, protocol.CertificateRequest{CSR: csr}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Heartbeat(ctx context.Context, req protocol.HeartbeatRequest) (*protocol.HeartbeatResponse, error) {
	var out protocol.HeartbeatResponse
	if _, err := c.do(ctx, http.MethodPut, "/v1/agent/heartbeat", "", defaultTimeout, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollActions waits up to wait seconds for an action (long polling).
func (c *Client) PollActions(ctx context.Context, wait int) (*protocol.ActionsResponse, error) {
	var out protocol.ActionsResponse
	path := "/v1/agent/actions?wait=" + url.QueryEscape(fmt.Sprint(wait))
	timeout := time.Duration(wait)*time.Second + 15*time.Second
	if _, err := c.do(ctx, http.MethodGet, path, "", timeout, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Claim takes ownership of an action: only a successful claim authorizes execution.
func (c *Client) Claim(ctx context.Context, actionID string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/agent/actions/"+url.PathEscape(actionID)+"/claim", "", defaultTimeout, nil, nil)
	return err
}

func (c *Client) SendResult(ctx context.Context, actionID string, res protocol.Result) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/agent/actions/"+url.PathEscape(actionID)+"/result", "", defaultTimeout, res, nil)
	return err
}

// PutStatus sends the cluster status summary; the backend keeps only the latest.
func (c *Client) PutStatus(ctx context.Context, s protocol.ClusterStatus) error {
	_, err := c.do(ctx, http.MethodPut, "/v1/agent/status", "", defaultTimeout, s, nil)
	return err
}
