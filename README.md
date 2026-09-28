# ThumbOps agent (prototipo)

Agente che gira in ogni cluster Kubernetes gestito da ThumbOps. Si collega in
uscita al backend, riceve le azioni approvate nell'app e le esegue, entro i
limiti della policy locale del cluster.

Il prototipo implementa il protocollo v1 descritto nel documento di progetto:
registrazione con token di bootstrap e mTLS, heartbeat, long polling, claim,
invio dell'esito e rinnovo del certificato. Le azioni sono cinque:
`rollout-restart`, `scale`, `cordon`, `uncordon`, `drain`.

## Scelte del prototipo

- **Solo libreria standard di Go.** L'agente usa direttamente l'API REST di
  Kubernetes (merge patch, Eviction API, SelfSubjectAccessReview). Niente
  dipendenze esterne: il codice è piccolo e facile da rivedere. Per la
  dashboard servirà client-go (informer), che si potrà introdurre senza
  cambiare protocollo né logica delle azioni.
- **Idempotenza con annotazioni.** Ogni risorsa modificata riceve
  `thumbops.mobiletechnologies.cloud/last-action-id` e
  `.../last-action-result`, nella stessa patch della modifica. Se l'agente si
  riavvia prima di inviare l'esito, alla nuova consegna lo reinvia senza
  ripetere l'azione.
- **Scale su una sola patch del deployment**, così repliche e annotazioni
  cambiano insieme.
- **Drain come `kubectl drain`**: ignora DaemonSet, pod statici e terminati; si
  ferma *prima* del cordon se trova pod senza controller o con volumi
  `emptyDir` (questi ultimi solo con consenso esplicito); usa l'Eviction API,
  quindi rispetta i PodDisruptionBudget, e riprova i pod bloccati a turni fino
  al timeout.
- **Policy locale in JSON** (ConfigMap). Senza file la policy nega tutto. Oltre
  a tipi di azione, namespace e massimo di repliche, protegge per default i
  nodi del control plane.
- **Rotazione del certificato senza riavvio.** Il certificato client si
  presenta solo all'handshake TLS: dopo registrazione e rinnovo l'agente
  riapre le connessioni verso il backend. (Un test lo verifica: senza questo
  passaggio l'agente sarebbe stato rifiutato subito dopo la registrazione.)

## Struttura

```
cmd/thumbops-agent   comando dell'agente
cmd/mock-backend     backend finto per lo sviluppo locale (HTTP, o HTTPS con mTLS)
internal/protocol    messaggi del protocollo
internal/kube        client REST minimo per Kubernetes
internal/actions     esecuzione delle azioni
internal/policy      policy locale del cluster
internal/backend     client del backend
internal/identity    chiave Ed25519, CSR, certificato
internal/enroll      registrazione e rinnovo
internal/agent       ciclo principale
internal/kubefake    API server simulato (test)
internal/mockbackend backend simulato (test e sviluppo)
deploy/agent.yaml    manifest: RBAC, policy, Deployment
```

## Test

```
go test -race ./...
```

I test usano un API server simulato e un backend simulato, in memoria. Coprono
tra l'altro: idempotenza, drain con PDB, emptyDir e pod senza controller,
timeout del drain, rifiuti della policy, nodi del control plane, azioni
scadute, claim già presi, orologio sfasato, retry dell'esito, 401, 426,
indisponibilità del backend, registrazione, token monouso, mTLS e rinnovo del
certificato.

## Provarlo su un cluster kind

Serve un cluster di prova: **non usare un cluster di produzione.**

```
kind create cluster --name thumbops
kubectl create deployment web --image=nginx --replicas=2

# 1. backend finto (HTTP, nessuna autenticazione)
go run ./cmd/mock-backend -addr 127.0.0.1:8080

# 2. agente fuori dal cluster, in modalità sviluppo
kubectl proxy --port 8001 &
echo '{"allowed_actions":["rollout-restart","scale","cordon","uncordon","drain"],"denied_namespaces":["kube-system"],"max_replicas":10}' > /tmp/policy.json
go run ./cmd/thumbops-agent --dev-insecure \
  --backend-url http://127.0.0.1:8080 \
  --kube-api http://127.0.0.1:8001 \
  --policy-file /tmp/policy.json

# 3. azioni, come se fossero state approvate nell'app
curl -X POST 127.0.0.1:8080/debug/actions \
  -d '{"type":"scale","params":{"namespace":"default","deployment":"web","replicas":4}}'
curl -X POST 127.0.0.1:8080/debug/actions \
  -d '{"type":"drain","params":{"node":"thumbops-control-plane","timeout_seconds":60}}'
curl 127.0.0.1:8080/debug/actions   # stato ed esiti
```

Il drain dell'unico nodo di kind viene rifiutato perché è un nodo del control
plane: è il comportamento atteso. Con `kubectl proxy` l'agente usa le tue
credenziali; per provare i permessi reali del ServiceAccount:

```
kubectl apply -f deploy/agent.yaml
TOKEN=$(kubectl -n thumbops create token thumbops-agent)
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > /tmp/kind-ca.crt
go run ./cmd/thumbops-agent --dev-insecure --backend-url http://127.0.0.1:8080 \
  --kube-api "$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')" \
  --kube-token "$TOKEN" --kube-ca-file /tmp/kind-ca.crt --policy-file /tmp/policy.json
```

### Registrazione, mTLS e rinnovo

Con `-tls-cert` e `-tls-key` il backend finto serve HTTPS come quello reale:
token di bootstrap monouso, mTLS obbligatorio su `/v1/agent/*`, rinnovo del
certificato. Con `-cert-lifetime` corto il rinnovo scatta dopo pochi secondi
(quando resta meno di un terzo della validità, controllato a ogni heartbeat).
La CA dei certificati client si rigenera a ogni avvio del backend finto: dopo
un suo riavvio l'agente va registrato di nuovo con uno `--state-dir` vuoto.

```
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj /CN=mock-backend \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" -keyout /tmp/server.key -out /tmp/server.crt
go run ./cmd/mock-backend -addr 127.0.0.1:8443 -tls-cert /tmp/server.crt -tls-key /tmp/server.key \
  -bootstrap-token segreto-di-prova -cert-lifetime 90s
echo segreto-di-prova > /tmp/bootstrap-token
go run ./cmd/thumbops-agent --backend-url https://127.0.0.1:8443 --backend-ca-file /tmp/server.crt \
  --bootstrap-token-file /tmp/bootstrap-token --state-dir /tmp/thumbops-state --heartbeat-interval 10s \
  --kube-api http://127.0.0.1:8001 --policy-file /tmp/policy.json
curl --cacert /tmp/server.crt https://127.0.0.1:8443/debug/actions
```

## Cosa manca per la produzione

- Stato del cluster per la dashboard (`PUT /v1/agent/status`, con informer).
- Chiave e certificato in un Secret invece che su un volume.
- Metriche Prometheus dell'agente e probe di liveness.
- Test su cluster reali (kind in CI) oltre a quelli con l'API simulata.
- Esito intermedio per i drain lunghi (questione aperta nel protocollo).
