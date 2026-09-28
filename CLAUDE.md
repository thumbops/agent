# ThumbOps agent

Agente Go che gira in ogni cluster: riceve dal backend le azioni approvate e le
esegue entro la policy locale del cluster. Contesto generale in `../CLAUDE.md`,
protocollo in `../docs/protocollo.md`.

## Comandi

```
go test -race ./...     # tutti i test (API server e backend simulati)
go vet ./...
gofmt -l .              # deve restituire nulla
go run ./cmd/mock-backend -addr 127.0.0.1:8080
go run ./cmd/thumbops-agent --dev-insecure --backend-url http://127.0.0.1:8080 --kube-api http://127.0.0.1:8001 --policy-file /tmp/policy.json
```

Per provarlo su kind vedi `README.md`. Mai su un cluster di produzione.

## Struttura

| Pacchetto | Ruolo |
| --- | --- |
| `cmd/thumbops-agent` | Flag, avvio, registrazione iniziale |
| `internal/protocol` | Tipi dei messaggi del protocollo v1 |
| `internal/kube` | Client REST minimo per Kubernetes (solo libreria standard) |
| `internal/actions` | Esecuzione di rollout-restart, scale, cordon, uncordon, drain |
| `internal/policy` | Policy locale del cluster (JSON da ConfigMap) |
| `internal/backend` | Client HTTP del backend |
| `internal/identity` | Chiave Ed25519, CSR, certificato su disco |
| `internal/enroll` | Registrazione e rinnovo del certificato |
| `internal/agent` | Ciclo principale: heartbeat, poll, claim, esecuzione, esito |
| `internal/kubefake` | API server simulato per i test |
| `internal/mockbackend` | Backend simulato per test e sviluppo |

## Perché niente client-go (per ora)

Il prototipo è stato scritto in un ambiente senza accesso a proxy.golang.org,
quindi usa solo la libreria standard. Non è una scelta di design da difendere:
client-go va introdotto per lo stato del cluster (informer), ed è accettabile
migrare anche le azioni se semplifica il codice. In quel caso i test con
`kubefake` vanno sostituiti o affiancati da test con il fake clientset o envtest.

## Invarianti da non rompere

Ognuna è coperta da test; se un cambiamento li fa fallire, fermati e capisci perché.

- **Claim prima di eseguire.** Un'azione si esegue solo dopo un claim riuscito
  (`200`); `409`/`410` = scartare. Le azioni scadute non si reclamano nemmeno.
- **Scadenze con l'orologio del backend.** L'agente corregge lo sfasamento con
  `server_time` dell'heartbeat.
- **Idempotenza nella stessa patch.** Le annotazioni `last-action-id` e
  `last-action-result` vanno scritte nella stessa merge patch che applica la
  modifica. Se trovi l'ID già presente, reinvia l'esito salvato.
- **Policy default deny.** Senza file di policy tutto è rifiutato; campi
  sconosciuti nella policy sono un errore di avvio. I nodi del control plane
  sono protetti salvo `allow_control_plane_nodes`.
- **Drain sicuro.** Blocca *prima* del cordon se ci sono pod senza controller o
  con `emptyDir` (senza consenso); usa l'Eviction API (rispetta i PDB); ignora
  DaemonSet, pod statici e terminati; a timeout il nodo resta in cordon e
  l'esito elenca i pod rimasti.
- **Esito mai perso.** L'invio del risultato si ritenta con backoff finché il
  backend conferma (salvo `400`/`409`/`410`).
- **Cambio di certificato = nuove connessioni.** Dopo registrazione e rinnovo va
  chiamato `backend.Client.ResetConnections()` (lo fa `enroll.Activate`): il
  certificato client si presenta solo all'handshake e il long polling tiene la
  connessione sempre attiva. Questo bug è già stato trovato una volta.
- **`401` ferma l'agente** (`ErrUnauthorized`); `426` lo lascia in sola modalità heartbeat.

## Prossimi passi

1. Prova su kind con il backend finto; poi con il token del ServiceAccount per verificare l'RBAC reale.
2. Stato del cluster: `PUT /v1/agent/status` ogni 60 s e su `status_requested`, con informer; formato e limiti in `../docs/protocollo.md` (sezione "Stato del cluster"). Rispettare `status.exclude_namespaces` della policy. ClusterRole separato in sola lettura.
3. Chiave e certificato in un Secret gestito dall'agente invece che su PVC.
4. Helm chart (sostituisce `deploy/agent.yaml`), probe di liveness/readiness, metriche Prometheus.
5. CI (GitHub Actions): `go test -race`, `go vet`, `gofmt`, test end-to-end su kind; build dell'immagine.
6. Aggiungere il file `LICENSE` (Apache 2.0).
