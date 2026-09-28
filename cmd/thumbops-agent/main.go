// Command thumbops-agent: the ThumbOps agent that runs in every cluster.
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

// version is set at build time with -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	var (
		backendURL  = flag.String("backend-url", "https://agent.thumbops.mobiletechnologies.cloud", "backend URL")
		backendCA   = flag.String("backend-ca-file", "", "backend server CA (empty = system CAs)")
		stateDir    = flag.String("state-dir", "/var/lib/thumbops", "directory for the private key and certificate")
		tokenFile   = flag.String("bootstrap-token-file", "/etc/thumbops/bootstrap/token", "single-use bootstrap token for the first registration")
		policyFile  = flag.String("policy-file", "/etc/thumbops/policy/policy.json", "cluster local policy (JSON)")
		devInsecure = flag.Bool("dev-insecure", false, "DEVELOPMENT ONLY: backend over HTTP, no registration and no mTLS")
		kubeAPI     = flag.String("kube-api", "", "DEVELOPMENT ONLY: API server URL when running outside the cluster")
		kubeToken   = flag.String("kube-token", "", "DEVELOPMENT ONLY: token for --kube-api")
		kubeCA      = flag.String("kube-ca-file", "", "DEVELOPMENT ONLY: CA for --kube-api")
		logLevel    = flag.String("log-level", "info", "debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print the version and exit")
		hbInterval  = flag.Duration("heartbeat-interval", 60*time.Second, "interval between heartbeats")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("invalid log level: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	kcfg := kube.Config{Host: *kubeAPI, Token: *kubeToken, CAFile: *kubeCA}
	if *kubeAPI == "" {
		var err error
		if kcfg, err = kube.InClusterConfig(); err != nil {
			fatal("%v (outside the cluster use --kube-api)", err)
		}
	}
	k, err := kube.New(kcfg)
	if err != nil {
		fatal("Kubernetes client: %v", err)
	}

	pol, err := policy.Load(*policyFile)
	if err != nil {
		fatal("%v", err)
	}
	if len(pol.AllowedActions) == 0 {
		log.Warn("local policy missing or empty: every action will be rejected", "policy_file", *policyFile)
	}

	userAgent := "thumbops-agent/" + version
	cfg := agent.Config{Version: version, HeartbeatInterval: *hbInterval, Logger: log}

	var b *backend.Client
	if *devInsecure {
		log.Warn("development mode: no authentication to the backend")
		b = backend.New(backend.Options{BaseURL: *backendURL, UserAgent: userAgent})
	} else {
		if !strings.HasPrefix(*backendURL, "https://") {
			fatal("the backend must be reached over HTTPS (for development: --dev-insecure)")
		}
		var roots *x509.CertPool
		if *backendCA != "" {
			pem, err := os.ReadFile(*backendCA)
			if err != nil {
				fatal("reading the backend CA: %v", err)
			}
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				fatal("no valid certificate in %s", *backendCA)
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
				fatal("registration failed: %v", err)
			}
			log.Info("agent registered", "cluster_id", store.ClusterID())
		}
		cfg.Renew = func(ctx context.Context) error {
			renewed, err := enroll.Renew(ctx, b, store, holder, time.Now())
			if renewed {
				log.Info("certificate renewed", "expires", holder.Get().Leaf.NotAfter)
			}
			return err
		}
		log.Info("identity loaded", "cluster_id", store.ClusterID(), "certificate_expires", holder.Get().Leaf.NotAfter)
	}

	a := agent.New(cfg, b, k, actions.New(k), pol)
	log.Info("agent started", "version", version, "backend", *backendURL)
	if err := a.Run(ctx); err != nil {
		fatal("%v", err)
	}
	log.Info("agent stopped")
}

func register(ctx context.Context, b *backend.Client, k *kube.Client, store identity.Store, holder *identity.Holder, tokenFile string) error {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("cannot read the bootstrap token: %w", err)
	}
	ver, err := k.ServerVersion(ctx)
	if err != nil {
		return fmt.Errorf("Kubernetes version: %w", err)
	}
	uid, err := k.NamespaceUID(ctx, "kube-system")
	if err != nil {
		return fmt.Errorf("kube-system UID: %w", err)
	}
	return enroll.Register(ctx, b, store, holder, string(token), protocol.RegisterRequest{
		AgentVersion: version, KubernetesVersion: ver, ClusterUID: uid,
	})
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
