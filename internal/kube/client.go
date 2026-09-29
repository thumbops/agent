// Package kube is a minimal client for the Kubernetes REST API, written with the
// standard library only. It covers only the calls the agent needs.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Config describes how to reach the API server.
type Config struct {
	Host      string // e.g. https://10.96.0.1:443
	Token     string // static token (only for development outside the cluster)
	TokenFile string // ServiceAccount token, re-read on every request because it rotates
	CAFile    string
	Timeout   time.Duration
}

// InClusterConfig reads the standard configuration of a pod.
func InClusterConfig() (Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return Config{}, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT not set")
	}
	return Config{
		Host:      "https://" + net.JoinHostPort(host, port),
		TokenFile: serviceAccountDir + "/token",
		CAFile:    serviceAccountDir + "/ca.crt",
	}, nil
}

// InClusterNamespace returns the namespace of the pod's ServiceAccount.
func InClusterNamespace() (string, error) {
	b, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil {
		return "", fmt.Errorf("pod namespace: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Client talks to the API server.
type Client struct {
	host string
	cfg  Config
	http *http.Client
}

func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, errors.New("missing API server host")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the API server is reached directly, never through a proxy
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading the API server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no valid certificate in the API server CA")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{host: strings.TrimRight(cfg.Host, "/"), cfg: cfg, http: &http.Client{Transport: tr, Timeout: timeout}}, nil
}

// APIError is an error returned by the API server.
type APIError struct {
	Code    int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes API %d %s: %s", e.Code, e.Reason, e.Message)
}

func code(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 0
}

func IsNotFound(err error) bool        { return code(err) == http.StatusNotFound }
func IsForbidden(err error) bool       { return code(err) == http.StatusForbidden }
func IsTooManyRequests(err error) bool { return code(err) == http.StatusTooManyRequests }

func (c *Client) token() (string, error) {
	if c.cfg.TokenFile != "" {
		b, err := os.ReadFile(c.cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("reading the ServiceAccount token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.cfg.Token, nil
}

const (
	contentJSON       = "application/json"
	contentMergePatch = "application/merge-patch+json"
)

func (c *Client) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.host+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", contentJSON)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Code: resp.StatusCode}
		var st struct{ Reason, Message string }
		if json.Unmarshal(data, &st) == nil {
			ae.Reason, ae.Message = st.Reason, st.Message
		}
		if ae.Message == "" {
			ae.Message = strings.TrimSpace(string(data))
		}
		return ae
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

func deploymentPath(ns, name string) string {
	return fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", esc(ns), esc(name))
}

func (c *Client) GetDeployment(ctx context.Context, ns, name string) (*Deployment, error) {
	var d Deployment
	if err := c.do(ctx, http.MethodGet, deploymentPath(ns, name), "", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// PatchDeployment applies a JSON merge patch.
func (c *Client) PatchDeployment(ctx context.Context, ns, name string, patch any) (*Deployment, error) {
	var d Deployment
	if err := c.do(ctx, http.MethodPatch, deploymentPath(ns, name), contentMergePatch, patch, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (c *Client) GetNode(ctx context.Context, name string) (*Node, error) {
	var n Node
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+esc(name), "", nil, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func (c *Client) PatchNode(ctx context.Context, name string, patch any) (*Node, error) {
	var n Node
	if err := c.do(ctx, http.MethodPatch, "/api/v1/nodes/"+esc(name), contentMergePatch, patch, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var l struct {
		Items []Node `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes", "", nil, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (c *Client) ListPodsOnNode(ctx context.Context, node string) ([]Pod, error) {
	var l struct {
		Items []Pod `json:"items"`
	}
	path := "/api/v1/pods?fieldSelector=" + url.QueryEscape("spec.nodeName="+node)
	if err := c.do(ctx, http.MethodGet, path, "", nil, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (c *Client) GetPod(ctx context.Context, ns, name string) (*Pod, error) {
	var p Pod
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", esc(ns), esc(name))
	if err := c.do(ctx, http.MethodGet, path, "", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// EvictPod uses the Eviction API, which honors PodDisruptionBudgets
// (the API server responds 429 if the eviction would violate a PDB).
func (c *Client) EvictPod(ctx context.Context, ns, name string) error {
	body := map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]string{"name": name, "namespace": ns},
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", esc(ns), esc(name))
	return c.do(ctx, http.MethodPost, path, contentJSON, body, nil)
}

// ResourceAttributes describes a permission to check.
type ResourceAttributes struct {
	Verb        string `json:"verb"`
	Group       string `json:"group"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
}

// CanI checks one of the agent's permissions with a SelfSubjectAccessReview.
func (c *Client) CanI(ctx context.Context, attrs ResourceAttributes) (bool, error) {
	body := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec":       map[string]any{"resourceAttributes": attrs},
	}
	var out struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", contentJSON, body, &out); err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", "", nil, &v); err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// NamespaceUID returns the UID of a namespace: the kube-system one
// identifies the cluster even if the agent is reinstalled.
func (c *Client) NamespaceUID(ctx context.Context, name string) (string, error) {
	var ns struct {
		Metadata ObjectMeta `json:"metadata"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+esc(name), "", nil, &ns); err != nil {
		return "", err
	}
	return ns.Metadata.UID, nil
}
