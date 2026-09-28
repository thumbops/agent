// Command mock-backend: a fake backend for trying the agent locally,
// for example with a kind cluster. Development only.
//
// Over HTTP there is no authentication (agent with --dev-insecure):
//
//	go run ./cmd/mock-backend -addr :8080
//	curl -X POST localhost:8080/debug/actions -d '{"type":"scale","params":{"namespace":"demo","deployment":"web","replicas":3}}'
//	curl localhost:8080/debug/actions
//
// With -tls-cert and -tls-key it serves HTTPS like the real backend:
// registration with a single-use bootstrap token, mTLS required on
// /v1/agent/* and certificate renewal (use a short -cert-lifetime to see it
// happen). The client certificate CA is generated at every start: after a
// restart the agent must register again, with an empty state-dir.
//
//	go run ./cmd/mock-backend -addr 127.0.0.1:8443 -tls-cert server.crt -tls-key server.key -cert-lifetime 2m
//	curl --cacert server.crt https://127.0.0.1:8443/debug/actions
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"

	"github.com/thumbops/agent/internal/mockbackend"
)

func main() {
	var (
		addr     = flag.String("addr", ":8080", "listen address")
		certFile = flag.String("tls-cert", "", "server certificate (PEM): enables HTTPS and mTLS")
		keyFile  = flag.String("tls-key", "", "server certificate private key (PEM)")
		token    = flag.String("bootstrap-token", "", "single-use bootstrap token (empty = the mock backend default)")
		lifetime = flag.Duration("cert-lifetime", 0, "validity of the certificates issued to the agent (0 = default, 30 days)")
	)
	flag.Parse()
	if (*certFile == "") != (*keyFile == "") {
		log.Fatal("-tls-cert and -tls-key must be given together")
	}

	s := mockbackend.New()
	if *token != "" {
		s.BootstrapToken = *token
	}
	if *lifetime > 0 {
		s.CertLifetime = *lifetime
	}

	if *certFile == "" {
		log.Printf("mock backend on HTTP at %s (development only, no authentication)", *addr)
		log.Fatal(http.ListenAndServe(*addr, s.Handler()))
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("server certificate: %v", err)
	}
	s.RequireMTLS = true
	tlsCfg := s.TLSConfig()
	tlsCfg.Certificates = []tls.Certificate{cert}
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), TLSConfig: tlsCfg}
	log.Printf("mock backend on HTTPS at %s: mTLS required on /v1/agent/*, certificate validity %s (development only)", *addr, s.CertLifetime)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
