// Comando mock-backend: un backend finto per provare l'agente in locale,
// ad esempio con un cluster kind. Solo HTTP, nessuna autenticazione.
//
//	go run ./cmd/mock-backend -addr :8080
//	curl -X POST localhost:8080/debug/actions -d '{"type":"scale","params":{"namespace":"demo","deployment":"web","replicas":3}}'
//	curl localhost:8080/debug/actions
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/thumbops/agent/internal/mockbackend"
)

func main() {
	addr := flag.String("addr", ":8080", "indirizzo di ascolto")
	flag.Parse()
	s := mockbackend.New()
	log.Printf("mock backend in ascolto su %s (solo sviluppo, nessuna autenticazione)", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
