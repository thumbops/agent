// Command thumbops-agent: the ThumbOps agent that runs in every cluster.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/agent"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/health"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/metrics"
	"github.com/thumbops/agent/internal/policy"
	"github.com/thumbops/agent/internal/protocol"
	"github.com/thumbops/agent/internal/status"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// version is set at build time with -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	var (
		backendURL  = flag.String("backend-url", "https://agent.thumbops.mobiletechnologies.cloud", "backend URL")
		backendCA   = flag.String("backend-ca-file", "", "backend server CA (empty = system CAs)")
		stateSecret = flag.String("state-secret", "thumbops-agent-identity", "Secret for the private key and certificate, in the agent namespace")
		namespace   = flag.String("namespace", "", "agent namespace (empty = the pod namespace)")
		stateDir    = flag.String("state-dir", "", "DEVELOPMENT ONLY: directory for the private key and certificate instead of the Secret")
		tokenFile   = flag.String("bootstrap-token-file", "/etc/thumbops/bootstrap/token", "single-use bootstrap token for the first registration")
		policyFile  = flag.String("policy-file", "/etc/thumbops/policy/policy.json", "cluster local policy (JSON)")
		devInsecure = flag.Bool("dev-insecure", false, "DEVELOPMENT ONLY: backend over HTTP, no registration and no mTLS")
		kubeAPI     = flag.String("kube-api", "", "DEVELOPMENT ONLY: API server URL when running outside the cluster")
		kubeToken   = flag.String("kube-token", "", "DEVELOPMENT ONLY: token for --kube-api")
		kubeCA      = flag.String("kube-ca-file", "", "DEVELOPMENT ONLY: CA for --kube-api")
		logLevel    = flag.String("log-level", "info", "debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print the version and exit")
		hbInterval  = flag.Duration("heartbeat-interval", 60*time.Second, "interval between heartbeats")
		statusOn    = flag.Bool("status", true, "send the cluster status for the dashboard (needs the thumbops-agent-status ClusterRole)")
		statusEvery = flag.Duration("status-interval", 60*time.Second, "interval between cluster status summaries")
		httpAddr    = flag.String("http-addr", ":9090", "address for /healthz, /readyz and /metrics (empty = disabled)")
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
	klog.SetSlogLogger(log) // client-go logs through klog: keep them JSON like the rest

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New(version)
	hs := health.New(health.LivenessThreshold(*hbInterval), nil)
	if *httpAddr != "" {
		// Started before registration: liveness answers while the agent
		// registers, readiness answers 503 until Run starts.
		srv := &http.Server{Addr: *httpAddr, Handler: health.Handler(hs, m.Handler()), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal("health and metrics server: %v", err)
			}
		}()
		go func() {
			<-ctx.Done()
			srv.Close()
		}()
	}

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
	cfg := agent.Config{Version: version, HeartbeatInterval: *hbInterval, Logger: log, Metrics: m, Health: hs}

	// client-go reads the same connection settings; BearerTokenFile is
	// re-read because the ServiceAccount token rotates.
	cs, err := kubernetes.NewForConfig(&rest.Config{
		Host:            kcfg.Host,
		BearerToken:     kcfg.Token,
		BearerTokenFile: kcfg.TokenFile,
		TLSClientConfig: rest.TLSClientConfig{CAFile: kcfg.CAFile},
		UserAgent:       userAgent,
	})
	if err != nil {
		fatal("Kubernetes client: %v", err)
	}

	if *statusOn {
		collector := status.NewCollector(cs, pol.Status.ExcludeNamespaces)
		collector.Start(ctx)
		cfg.Status = collector
		cfg.StatusInterval = *statusEvery
	}

	var b *backend.Client
	if *devInsecure {
		log.Warn("development mode: no authentication to the backend")
		b = backend.New(backend.Options{BaseURL: *backendURL, UserAgent: userAgent, Metrics: m})
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
		var store identity.Store
		if *stateDir != "" {
			store = identity.FileStore{Dir: *stateDir}
		} else {
			ns := *namespace
			if ns == "" {
				if ns, err = kube.InClusterNamespace(); err != nil {
					fatal("%v (outside the cluster use --namespace or --state-dir)", err)
				}
			}
			store = identity.SecretStore{Client: cs, Namespace: ns, Name: *stateSecret}
		}
		holder := &identity.Holder{}
		b = backend.New(backend.Options{BaseURL: *backendURL, UserAgent: userAgent, TLS: holder.ClientTLS(roots), Metrics: m})
		en := &enroll.Enroller{Backend: b, Store: store, Holder: holder, Logger: log, Metrics: m}

		token, err := readToken(*tokenFile)
		if err != nil {
			fatal("%v", err)
		}
		registered, err := en.Start(ctx, token, func(ctx context.Context) (protocol.RegisterRequest, error) {
			return registerInfo(ctx, k)
		})
		if err != nil {
			fatal("identity: %v", err)
		}
		if registered {
			log.Info("agent registered", "cluster_id", en.ClusterID())
		}
		cfg.Renew = func(ctx context.Context) error {
			renewed, err := en.Renew(ctx, time.Now())
			if renewed {
				log.Info("certificate renewed", "expires", holder.Get().Leaf.NotAfter)
			}
			return err
		}
		log.Info("identity loaded", "cluster_id", en.ClusterID(), "certificate_expires", holder.Get().Leaf.NotAfter)
	}

	a := agent.New(cfg, b, k, actions.New(k), pol)
	log.Info("agent started", "version", version, "backend", *backendURL)
	if err := a.Run(ctx); err != nil {
		fatal("%v", err)
	}
	log.Info("agent stopped")
}

// readToken reads the bootstrap token; a missing file means no token (the
// bootstrap Secret is optional once the agent is registered).
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot read the bootstrap token: %w", err)
	}
	return string(b), nil
}

// registerInfo collects the cluster data sent at registration.
func registerInfo(ctx context.Context, k *kube.Client) (protocol.RegisterRequest, error) {
	ver, err := k.ServerVersion(ctx)
	if err != nil {
		return protocol.RegisterRequest{}, fmt.Errorf("Kubernetes version: %w", err)
	}
	uid, err := k.NamespaceUID(ctx, "kube-system")
	if err != nil {
		return protocol.RegisterRequest{}, fmt.Errorf("kube-system UID: %w", err)
	}
	return protocol.RegisterRequest{AgentVersion: version, KubernetesVersion: ver, ClusterUID: uid}, nil
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
