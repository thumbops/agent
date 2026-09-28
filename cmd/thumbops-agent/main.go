// Comando thumbops-agent: l'agente ThumbOps che gira in ogni cluster.
package main

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/agent"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/policy"
	"github.com/thumbops/agent/internal/protocol"
)

// version viene impostata in build con -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	var (
		backendURL  = flag.String("backend-url", "https://agent.thumbops.mobiletechnologies.cloud", "URL del backend")
		backendCA   = flag.String("backend-ca-file", "", "CA del server del backend (vuoto = CA di sistema)")
		stateDir    = flag.String("state-dir", "/var/lib/thumbops", "cartella per chiave privata e certificato")
		tokenFile   = flag.String("bootstrap-token-file", "/etc/thumbops/bootstrap/token", "token di bootstrap monouso per la prima registrazione")
		policyFile  = flag.String("policy-file", "/etc/thumbops/policy/policy.json", "policy locale del cluster (JSON)")
		devInsecure = flag.Bool("dev-insecure", false, "SOLO SVILUPPO: backend in HTTP, nessuna registrazione né mTLS")
		kubeAPI     = flag.String("kube-api", "", "SOLO SVILUPPO: URL dell'API server fuori dal cluster")
		kubeToken   = flag.String("kube-token", "", "SOLO SVILUPPO: token per --kube-api")
		kubeCA      = flag.String("kube-ca-file", "", "SOLO SVILUPPO: CA per --kube-api")
		logLevel    = flag.String("log-level", "info", "debug, info, warn, error")
		showVersion = flag.Bool("version", false, "stampa la versione ed esce")
		hbInterval  = flag.Duration("heartbeat-interval", 60*time.Second, "intervallo tra gli heartbeat")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("livello di log non valido: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	kcfg := kube.Config{Host: *kubeAPI, Token: *kubeToken, CAFile: *kubeCA}
	if *kubeAPI == "" {
		var err error
		if kcfg, err = kube.InClusterConfig(); err != nil {
			fatal("%v (fuori dal cluster usare --kube-api)", err)
		}
	}
	k, err := kube.New(kcfg)
	if err != nil {
		fatal("client Kubernetes: %v", err)
	}

	pol, err := policy.Load(*policyFile)
	if err != nil {
		fatal("%v", err)
	}
	if len(pol.AllowedActions) == 0 {
		log.Warn("policy locale assente o vuota: tutte le azioni verranno rifiutate", "policy_file", *policyFile)
	}

	userAgent := "thumbops-agent/" + version
	cfg := agent.Config{Version: version, HeartbeatInterval: *hbInterval, Logger: log}

	var b *backend.Client
	if *devInsecure {
		log.Warn("modalità di sviluppo: nessuna autenticazione verso il backend")
		b = backend.New(backend.Options{BaseURL: *backendURL, UserAgent: userAgent})
	} else {
		if !strings.HasPrefix(*backendURL, "https://") {
			fatal("il backend deve essere raggiungibile in HTTPS (per lo sviluppo: --dev-insecure)")
		}
		var roots *x509.CertPool
		if *backendCA != "" {
			pem, err := os.ReadFile(*backendCA)
			if err != nil {
				fatal("lettura CA del backend: %v", err)
			}
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				fatal("nessun certificato valido in %s", *backendCA)
			}
		}
		store := identity.Store{Dir: *stateDir}
		holder := &identity.Holder{}
		b = backend.New(backend.Options{BaseURL: *backendURL, UserAgent: userAgent, TLS: holder.ClientTLS(roots)})

		if store.Registered() {
			if err := enroll.Activate(b, store, holder); err != nil {
				fatal("%v", err)
			}
		} else {
			if err := register(ctx, b, k, store, holder, *tokenFile); err != nil {
				fatal("registrazione non riuscita: %v", err)
			}
			log.Info("agente registrato", "cluster_id", store.ClusterID())
		}
		cfg.Renew = func(ctx context.Context) error {
			renewed, err := enroll.Renew(ctx, b, store, holder, time.Now())
			if renewed {
				log.Info("certificato rinnovato", "expires", holder.Get().Leaf.NotAfter)
			}
			return err
		}
		log.Info("identità caricata", "cluster_id", store.ClusterID(), "certificate_expires", holder.Get().Leaf.NotAfter)
	}

	a := agent.New(cfg, b, k, actions.New(k), pol)
	log.Info("agente avviato", "version", version, "backend", *backendURL)
	if err := a.Run(ctx); err != nil {
		fatal("%v", err)
	}
	log.Info("agente arrestato")
}

func register(ctx context.Context, b *backend.Client, k *kube.Client, store identity.Store, holder *identity.Holder, tokenFile string) error {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("token di bootstrap non leggibile: %w", err)
	}
	ver, err := k.ServerVersion(ctx)
	if err != nil {
		return fmt.Errorf("versione di Kubernetes: %w", err)
	}
	uid, err := k.NamespaceUID(ctx, "kube-system")
	if err != nil {
		return fmt.Errorf("UID di kube-system: %w", err)
	}
	return enroll.Register(ctx, b, store, holder, string(token), protocol.RegisterRequest{
		AgentVersion: version, KubernetesVersion: ver, ClusterUID: uid,
	})
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
