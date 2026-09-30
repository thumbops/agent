// Package mockbackend is a minimal in-memory ThumbOps backend that follows
// protocol v1. It serves tests and local development; it is not the real backend.
package mockbackend

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thumbops/agent/internal/protocol"
)

const (
	stateApproved  = "approved"
	stateClaimed   = "claimed"
	stateDone      = "done"
	stateExpired   = "expired"
	stateCancelled = "cancelled"
)

type entry struct {
	action protocol.Action
	state  string

	leaseUntil    time.Time // a claimed action expires after this without a result
	progress      protocol.Progress
	progressCount int
}

type Server struct {
	mu      sync.Mutex
	entries []*entry
	byID    map[string]*entry
	results map[string]protocol.Result
	notify  chan struct{}

	heartbeats []protocol.HeartbeatRequest
	statuses   []protocol.ClusterStatus
	// statusRequested is returned once in the next poll response.
	statusRequested bool
	statusCode      int // if not 0, PUT /v1/agent/status responds with this status
	heartbeatCode   int // if not 0, PUT /v1/agent/heartbeat responds with this status

	// Behaviors configurable in tests.
	Poll           protocol.PollConfig
	ClaimOverride  map[string]int // action_id → HTTP status to return on claim
	PollStatus     int            // if not 0, polling responds with this status
	ResultFailures int            // how many times to respond 503 when the result is sent
	ClaimLease     time.Duration  // lease of a claimed action, renewed by progress
	BootstrapToken string
	RequireMTLS    bool // requires a valid client certificate on /v1/agent/*
	CertLifetime   time.Duration
	clusterID      string
	extraTokens    map[string]bool // AddBootstrapToken, for new registrations
	usedTokens     map[string]bool
	caCert         *x509.Certificate
	caKey          ed25519.PrivateKey
	caPEM          string
	registrations  []protocol.RegisterRequest
	renewals       int
}

func New() *Server {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ThumbOps mock agent CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		panic(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &Server{
		byID:           map[string]*entry{},
		results:        map[string]protocol.Result{},
		notify:         make(chan struct{}),
		Poll:           protocol.PollConfig{WaitSeconds: 20},
		ClaimOverride:  map[string]int{},
		extraTokens:    map[string]bool{},
		usedTokens:     map[string]bool{},
		BootstrapToken: "bootstrap-test-token",
		CertLifetime:   30 * 24 * time.Hour,
		ClaimLease:     5 * time.Minute,
		clusterID:      "8c1f0e7a-0000-4000-8000-00000000c1a5",
		caCert:         ca,
		caKey:          key,
		caPEM:          string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// ClientCAs is the pool to use as tls.Config.ClientCAs for mTLS.
func (s *Server) ClientCAs() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.caCert)
	return p
}

// TLSConfig configures a TLS server that asks for (without requiring) the client
// certificate: /v1/register works without it, /v1/agent/* requires it if RequireMTLS.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: s.ClientCAs()}
}

// Enqueue adds an already approved action.
func (s *Server) Enqueue(a protocol.Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueueLocked(a)
	close(s.notify)
	s.notify = make(chan struct{})
}

// enqueueLocked adds the action as approved and returns its entry, without
// waking the pollers. Call it with s.mu held.
func (s *Server) enqueueLocked(a protocol.Action) *entry {
	if a.ActionID == "" {
		a.ActionID = fmt.Sprintf("act-%d", len(s.entries)+1)
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = time.Now().Add(2 * time.Minute)
	}
	e := &entry{action: a, state: stateApproved}
	s.entries = append(s.entries, e)
	s.byID[a.ActionID] = e
	return e
}

// expireLeases marks as expired the claimed actions whose lease is over.
// Call it with s.mu held.
func (s *Server) expireLeases() {
	now := time.Now()
	for _, e := range s.entries {
		if e.state == stateClaimed && !e.leaseUntil.IsZero() && now.After(e.leaseUntil) {
			e.state = stateExpired
		}
	}
}

// EnqueueClaimed adds an action already claimed, as if the agent had claimed
// it before a restart.
func (s *Server) EnqueueClaimed(a protocol.Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.enqueueLocked(a)
	e.state = stateClaimed
	e.leaseUntil = time.Now().Add(s.ClaimLease)
}

// Cancel cancels an action that is approved or claimed (the next progress
// gets 410).
func (s *Server) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok && (e.state == stateApproved || e.state == stateClaimed) {
		e.state = stateCancelled
	}
}

// Progress returns the last progress of an action and how many were received.
func (s *Server) Progress(id string) (protocol.Progress, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return protocol.Progress{}, 0
	}
	return e.progress, e.progressCount
}

func (s *Server) progressHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p protocol.Progress
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLeases()
	e, ok := s.byID[id]
	switch {
	case !ok:
		http.Error(w, "unknown action", http.StatusNotFound)
	case e.state == stateClaimed:
		e.progress = p
		e.progressCount++
		e.leaseUntil = time.Now().Add(s.ClaimLease)
		w.WriteHeader(http.StatusOK)
	case e.state == stateExpired || e.state == stateCancelled:
		http.Error(w, "action expired or cancelled", http.StatusGone)
	default:
		http.Error(w, "action not claimed", http.StatusConflict)
	}
}

func (s *Server) debugCancel(w http.ResponseWriter, r *http.Request) {
	s.Cancel(r.PathValue("id"))
	w.WriteHeader(http.StatusOK)
}

// SetPollStatus changes the polling response even while the server is in use
// (0 = normal behavior).
func (s *Server) SetPollStatus(code int) {
	s.mu.Lock()
	s.PollStatus = code
	s.mu.Unlock()
}

// SetHeartbeatStatus makes the heartbeat respond with code (0 = normal).
func (s *Server) SetHeartbeatStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatCode = code
}

func (s *Server) Result(id string) (protocol.Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.results[id]
	return r, ok
}

// WaitResult waits for the result of an action.
func (s *Server) WaitResult(id string, timeout time.Duration) (protocol.Result, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r, ok := s.Result(id); ok {
			return r, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return protocol.Result{}, false
}

func (s *Server) State(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		return e.state
	}
	return ""
}

// RequestStatus asks the agent for a fresh status, as the app's refresh does:
// the next poll response carries status_requested.
func (s *Server) RequestStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusRequested = true
	close(s.notify)
	s.notify = make(chan struct{})
}

// SetStatusCode makes PUT /v1/agent/status respond with code (0 = normal).
func (s *Server) SetStatusCode(code int) {
	s.mu.Lock()
	s.statusCode = code
	s.mu.Unlock()
}

// Statuses returns the status summaries received so far.
func (s *Server) Statuses() []protocol.ClusterStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.ClusterStatus(nil), s.statuses...)
}

func (s *Server) Heartbeats() []protocol.HeartbeatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.HeartbeatRequest(nil), s.heartbeats...)
}

func (s *Server) Registrations() []protocol.RegisterRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.RegisterRequest(nil), s.registrations...)
}

// AddBootstrapToken accepts one more single-use token, as when the cluster
// gets a new token from the app to register again.
func (s *Server) AddBootstrapToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraTokens[token] = true
}

func (s *Server) Renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

// Handler exposes the protocol routes and, for development, the /debug routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/register", s.register)
	mux.HandleFunc("POST /v1/agent/certificate", s.renew)
	mux.HandleFunc("PUT /v1/agent/heartbeat", s.heartbeat)
	mux.HandleFunc("PUT /v1/agent/status", s.status)
	mux.HandleFunc("GET /v1/agent/actions", s.poll)
	mux.HandleFunc("POST /v1/agent/actions/{id}/claim", s.claim)
	mux.HandleFunc("POST /v1/agent/actions/{id}/result", s.result)
	mux.HandleFunc("POST /v1/agent/actions/{id}/progress", s.progressHandler)
	mux.HandleFunc("POST /debug/actions", s.debugEnqueue)
	mux.HandleFunc("GET /debug/actions", s.debugList)
	mux.HandleFunc("POST /debug/actions/{id}/cancel", s.debugCancel)
	mux.HandleFunc("POST /debug/bootstrap-tokens", s.debugAddToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.RequireMTLS && strings.HasPrefix(r.URL.Path, "/v1/agent/") {
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
				http.Error(w, "client certificate missing or invalid", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) issue(csrPEM string) (string, time.Time, error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", time.Time{}, fmt.Errorf("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", time.Time{}, fmt.Errorf("invalid CSR signature: %w", err)
	}
	now := time.Now()
	notAfter := now.Add(s.CertLifetime)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: s.clusterID}, // the CN is the cluster_id
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		return "", time.Time{}, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), notAfter, nil
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if (token != s.BootstrapToken && !s.extraTokens[token]) || s.usedTokens[token] {
		http.Error(w, "bootstrap token invalid or already used", http.StatusUnauthorized)
		return
	}
	cert, exp, err := s.issue(req.CSR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.usedTokens[token] = true
	s.registrations = append(s.registrations, req)
	writeJSON(w, http.StatusOK, protocol.RegisterResponse{ClusterID: s.clusterID, Certificate: cert, CAChain: s.caPEM, ExpiresAt: exp})
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	var req protocol.CertificateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cert, exp, err := s.issue(req.CSR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renewals++
	writeJSON(w, http.StatusOK, protocol.CertificateResponse{Certificate: cert, ExpiresAt: exp})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req protocol.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if code := s.heartbeatCode; code != 0 {
		s.mu.Unlock()
		http.Error(w, http.StatusText(code), code)
		return
	}
	s.heartbeats = append(s.heartbeats, req)
	poll := s.Poll
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, protocol.HeartbeatResponse{ServerTime: time.Now().UTC(), Poll: poll, MinAgentVersion: "0.1.0"})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var st protocol.ClusterStatus
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusCode != 0 {
		http.Error(w, http.StatusText(s.statusCode), s.statusCode)
		return
	}
	s.statuses = append(s.statuses, st)
	w.WriteHeader(http.StatusNoContent)
}

// next returns the first approved, unexpired action; must be called with the lock held.
func (s *Server) next() *protocol.Action {
	now := time.Now()
	for _, e := range s.entries {
		if e.state != stateApproved {
			continue
		}
		if now.After(e.action.ExpiresAt) {
			e.state = stateExpired
			continue
		}
		a := e.action
		return &a
	}
	return nil
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.PollStatus != 0 {
		code := s.PollStatus
		s.mu.Unlock()
		http.Error(w, http.StatusText(code), code)
		return
	}
	s.mu.Unlock()

	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		a := s.next()
		requested := s.statusRequested
		s.statusRequested = false
		ch := s.notify
		s.mu.Unlock()
		if a != nil {
			writeJSON(w, http.StatusOK, protocol.ActionsResponse{Actions: []protocol.Action{*a}, StatusRequested: requested})
			return
		}
		if requested {
			writeJSON(w, http.StatusOK, protocol.ActionsResponse{Actions: []protocol.Action{}, StatusRequested: true})
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLeases()
	e, ok := s.byID[id]
	if !ok {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if code := s.ClaimOverride[id]; code != 0 {
		e.state = stateCancelled // no longer offered
		http.Error(w, http.StatusText(code), code)
		return
	}
	switch {
	case e.state == stateApproved && time.Now().After(e.action.ExpiresAt):
		e.state = stateExpired
		http.Error(w, "action expired", http.StatusGone)
	case e.state == stateApproved:
		e.state = stateClaimed
		e.leaseUntil = time.Now().Add(s.ClaimLease)
		w.WriteHeader(http.StatusOK)
	case e.state == stateExpired || e.state == stateCancelled:
		http.Error(w, "action expired or canceled", http.StatusGone)
	default:
		http.Error(w, "action already claimed", http.StatusConflict)
	}
}

func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var res protocol.Result
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ResultFailures > 0 {
		s.ResultFailures--
		http.Error(w, "backend temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	s.expireLeases()
	e, ok := s.byID[id]
	if ok && (e.state == stateExpired || e.state == stateCancelled) {
		http.Error(w, "action expired or cancelled", http.StatusGone)
		return
	}
	if !ok || e.state != stateClaimed {
		http.Error(w, "action not claimed", http.StatusConflict)
		return
	}
	e.state = stateDone
	s.results[id] = res
	w.WriteHeader(http.StatusOK)
}

func (s *Server) debugEnqueue(w http.ResponseWriter, r *http.Request) {
	var a protocol.Action
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Enqueue(a)
	s.mu.Lock()
	queued := s.entries[len(s.entries)-1].action
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, queued)
}

func (s *Server) debugAddToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, `expected {"token": "..."}`, http.StatusBadRequest)
		return
	}
	s.AddBootstrapToken(req.Token)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) debugList(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLeases()
	type row struct {
		Action        protocol.Action    `json:"action"`
		State         string             `json:"state"`
		Result        *protocol.Result   `json:"result,omitempty"`
		Progress      *protocol.Progress `json:"progress,omitempty"`
		ProgressCount int                `json:"progress_count,omitempty"`
	}
	out := []row{}
	for _, e := range s.entries {
		rw := row{Action: e.action, State: e.state}
		if res, ok := s.results[e.action.ActionID]; ok {
			rw.Result = &res
		}
		if e.progressCount > 0 {
			p := e.progress
			rw.Progress = &p
			rw.ProgressCount = e.progressCount
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, out)
}
