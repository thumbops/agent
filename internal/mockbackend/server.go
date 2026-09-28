// Package mockbackend è un backend ThumbOps minimo, in memoria, che segue il
// protocollo v1. Serve ai test e allo sviluppo locale; non è il backend reale.
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
}

type Server struct {
	mu      sync.Mutex
	entries []*entry
	byID    map[string]*entry
	results map[string]protocol.Result
	notify  chan struct{}

	heartbeats []protocol.HeartbeatRequest

	// Comportamenti configurabili nei test.
	Poll           protocol.PollConfig
	ClaimOverride  map[string]int // action_id → codice HTTP da restituire al claim
	PollStatus     int            // se diverso da 0, il polling risponde con questo codice
	ResultFailures int            // quante volte rispondere 503 all'invio dell'esito
	BootstrapToken string
	RequireMTLS    bool // richiede un certificato client valido su /v1/agent/*
	CertLifetime   time.Duration
	clusterID      string
	tokenUsed      bool
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
		BootstrapToken: "bootstrap-test-token",
		CertLifetime:   30 * 24 * time.Hour,
		clusterID:      "8c1f0e7a-0000-4000-8000-00000000c1a5",
		caCert:         ca,
		caKey:          key,
		caPEM:          string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// ClientCAs è il pool da usare come tls.Config.ClientCAs per il mTLS.
func (s *Server) ClientCAs() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.caCert)
	return p
}

// TLSConfig configura un server TLS che chiede (senza imporlo) il certificato
// client: /v1/register funziona senza, /v1/agent/* lo richiede se RequireMTLS.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: s.ClientCAs()}
}

// Enqueue aggiunge un'azione già approvata.
func (s *Server) Enqueue(a protocol.Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ActionID == "" {
		a.ActionID = fmt.Sprintf("act-%d", len(s.entries)+1)
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = time.Now().Add(2 * time.Minute)
	}
	e := &entry{action: a, state: stateApproved}
	s.entries = append(s.entries, e)
	s.byID[a.ActionID] = e
	close(s.notify)
	s.notify = make(chan struct{})
}

// SetPollStatus cambia la risposta del polling anche mentre il server è in uso
// (0 = comportamento normale).
func (s *Server) SetPollStatus(code int) {
	s.mu.Lock()
	s.PollStatus = code
	s.mu.Unlock()
}

func (s *Server) Result(id string) (protocol.Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.results[id]
	return r, ok
}

// WaitResult attende l'esito di un'azione.
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

func (s *Server) Heartbeats() []protocol.HeartbeatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.HeartbeatRequest(nil), s.heartbeats...)
}

func (s *Server) Renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

// Handler espone le rotte del protocollo e, per lo sviluppo, due rotte /debug.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/register", s.register)
	mux.HandleFunc("POST /v1/agent/certificate", s.renew)
	mux.HandleFunc("PUT /v1/agent/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /v1/agent/actions", s.poll)
	mux.HandleFunc("POST /v1/agent/actions/{id}/claim", s.claim)
	mux.HandleFunc("POST /v1/agent/actions/{id}/result", s.result)
	mux.HandleFunc("POST /debug/actions", s.debugEnqueue)
	mux.HandleFunc("GET /debug/actions", s.debugList)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.RequireMTLS && strings.HasPrefix(r.URL.Path, "/v1/agent/") {
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
				http.Error(w, "certificato client mancante o non valido", http.StatusUnauthorized)
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
		return "", time.Time{}, fmt.Errorf("CSR non valida")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", time.Time{}, fmt.Errorf("firma della CSR non valida: %w", err)
	}
	now := time.Now()
	notAfter := now.Add(s.CertLifetime)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: s.clusterID}, // il CN è il cluster_id
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
	if r.Header.Get("Authorization") != "Bearer "+s.BootstrapToken || s.tokenUsed {
		http.Error(w, "token di bootstrap non valido o già usato", http.StatusUnauthorized)
		return
	}
	cert, exp, err := s.issue(req.CSR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.tokenUsed = true
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
	s.heartbeats = append(s.heartbeats, req)
	poll := s.Poll
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, protocol.HeartbeatResponse{ServerTime: time.Now().UTC(), Poll: poll, MinAgentVersion: "0.1.0"})
}

// next restituisce la prima azione approvata e non scaduta; va chiamata con il lock.
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
		ch := s.notify
		s.mu.Unlock()
		if a != nil {
			writeJSON(w, http.StatusOK, protocol.ActionsResponse{Actions: []protocol.Action{*a}})
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
	e, ok := s.byID[id]
	if !ok {
		http.Error(w, "azione sconosciuta", http.StatusNotFound)
		return
	}
	if code := s.ClaimOverride[id]; code != 0 {
		e.state = stateCancelled // non viene più riproposta
		http.Error(w, http.StatusText(code), code)
		return
	}
	switch {
	case e.state == stateApproved && time.Now().After(e.action.ExpiresAt):
		e.state = stateExpired
		http.Error(w, "azione scaduta", http.StatusGone)
	case e.state == stateApproved:
		e.state = stateClaimed
		w.WriteHeader(http.StatusOK)
	case e.state == stateExpired || e.state == stateCancelled:
		http.Error(w, "azione scaduta o annullata", http.StatusGone)
	default:
		http.Error(w, "azione già presa in carico", http.StatusConflict)
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
		http.Error(w, "backend temporaneamente non disponibile", http.StatusServiceUnavailable)
		return
	}
	e, ok := s.byID[id]
	if !ok || e.state != stateClaimed {
		http.Error(w, "azione non presa in carico", http.StatusConflict)
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

func (s *Server) debugList(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct {
		Action protocol.Action  `json:"action"`
		State  string           `json:"state"`
		Result *protocol.Result `json:"result,omitempty"`
	}
	out := []row{}
	for _, e := range s.entries {
		rw := row{Action: e.action, State: e.state}
		if res, ok := s.results[e.action.ActionID]; ok {
			rw.Result = &res
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, out)
}
