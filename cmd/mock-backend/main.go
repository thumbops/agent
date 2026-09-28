// Comando mock-backend: un backend finto per provare l'agente in locale,
// ad esempio con un cluster kind. Solo per lo sviluppo.
//
// In HTTP non c'è autenticazione (agente con --dev-insecure):
//
//	go run ./cmd/mock-backend -addr :8080
//	curl -X POST localhost:8080/debug/actions -d '{"type":"scale","params":{"namespace":"demo","deployment":"web","replicas":3}}'
//	curl localhost:8080/debug/actions
//
// Con -tls-cert e -tls-key serve HTTPS come il backend reale: registrazione
// con token di bootstrap monouso, mTLS obbligatorio su /v1/agent/* e rinnovo
// del certificato (-cert-lifetime corto per vederlo scattare). La CA dei
// certificati client viene generata a ogni avvio: dopo un riavvio l'agente
// va registrato di nuovo, con uno state-dir vuoto.
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
		addr     = flag.String("addr", ":8080", "indirizzo di ascolto")
		certFile = flag.String("tls-cert", "", "certificato del server (PEM): abilita HTTPS e mTLS")
		keyFile  = flag.String("tls-key", "", "chiave privata del certificato del server (PEM)")
		token    = flag.String("bootstrap-token", "", "token di bootstrap monouso (vuoto = predefinito del backend finto)")
		lifetime = flag.Duration("cert-lifetime", 0, "validità dei certificati emessi all'agente (0 = predefinita, 30 giorni)")
	)
	flag.Parse()
	if (*certFile == "") != (*keyFile == "") {
		log.Fatal("-tls-cert e -tls-key vanno indicati insieme")
	}

	s := mockbackend.New()
	if *token != "" {
		s.BootstrapToken = *token
	}
	if *lifetime > 0 {
		s.CertLifetime = *lifetime
	}

	if *certFile == "" {
		log.Printf("mock backend in HTTP su %s (solo sviluppo, nessuna autenticazione)", *addr)
		log.Fatal(http.ListenAndServe(*addr, s.Handler()))
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("certificato del server: %v", err)
	}
	s.RequireMTLS = true
	tlsCfg := s.TLSConfig()
	tlsCfg.Certificates = []tls.Certificate{cert}
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), TLSConfig: tlsCfg}
	log.Printf("mock backend in HTTPS su %s: mTLS obbligatorio su /v1/agent/*, validità certificati %s (solo sviluppo)", *addr, s.CertLifetime)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
