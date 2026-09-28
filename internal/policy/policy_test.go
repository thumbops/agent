package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thumbops/agent/internal/protocol"
)

func intp(v int) *int { return &v }

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const example = `{
  "allowed_actions": ["rollout-restart", "scale", "cordon", "drain"],
  "denied_namespaces": ["kube-system"],
  "max_replicas": 20
}`

func TestMissingFileDeniesEverything(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "assente.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Check(protocol.Action{Type: protocol.ActionRolloutRestart, Params: protocol.Params{Namespace: "a", Deployment: "b"}})
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("attesa una violazione, ottenuto %v", err)
	}
}

func TestInvalidFiles(t *testing.T) {
	for name, content := range map[string]string{
		"campo sconosciuto":  `{"allowed_actions": ["scale"], "max_replica": 3}`,
		"azione sconosciuta": `{"allowed_actions": ["delete-namespace"]}`,
		"json rotto":         `{"allowed_actions": [`,
		"max negativo":       `{"allowed_actions": ["scale"], "max_replicas": -1}`,
	} {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: attesa un errore di caricamento", name)
		}
	}
}

func TestCheck(t *testing.T) {
	p, err := Load(write(t, example))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		action protocol.Action
		reason string // vuoto = ammessa
	}{
		{"restart ammesso", protocol.Action{Type: "rollout-restart", Params: protocol.Params{Namespace: "payments", Deployment: "api"}}, ""},
		{"scale entro il limite", protocol.Action{Type: "scale", Params: protocol.Params{Namespace: "payments", Deployment: "api", Replicas: intp(20)}}, ""},
		{"scale oltre il limite", protocol.Action{Type: "scale", Params: protocol.Params{Namespace: "payments", Deployment: "api", Replicas: intp(21)}}, "superano il massimo"},
		{"scale senza repliche", protocol.Action{Type: "scale", Params: protocol.Params{Namespace: "payments", Deployment: "api"}}, "repliche mancante"},
		{"namespace negato", protocol.Action{Type: "rollout-restart", Params: protocol.Params{Namespace: "kube-system", Deployment: "coredns"}}, "escluso"},
		{"azione non ammessa", protocol.Action{Type: "uncordon", Params: protocol.Params{Node: "w1"}}, "non ammessa"},
		{"drain con emptyDir", protocol.Action{Type: "drain", Params: protocol.Params{Node: "w1", DeleteEmptyDirData: true}}, "emptyDir"},
		{"drain senza nodo", protocol.Action{Type: "drain"}, "node mancante"},
	}
	for _, c := range cases {
		err := p.Check(c.action)
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: attesa ammessa, rifiutata: %v", c.name, err)
		case c.reason != "" && (err == nil || !strings.Contains(err.Error(), c.reason)):
			t.Errorf("%s: atteso rifiuto con %q, ottenuto %v", c.name, c.reason, err)
		}
	}
}

func TestAllowedNamespaces(t *testing.T) {
	p, err := Load(write(t, `{"allowed_actions": ["rollout-restart"], "allowed_namespaces": ["payments"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check(protocol.Action{Type: "rollout-restart", Params: protocol.Params{Namespace: "payments", Deployment: "api"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(protocol.Action{Type: "rollout-restart", Params: protocol.Params{Namespace: "orders", Deployment: "api"}}); err == nil {
		t.Fatal("namespace non in lista: atteso rifiuto")
	}
}

func TestControlPlaneNodes(t *testing.T) {
	p := &Policy{}
	cp := map[string]string{"node-role.kubernetes.io/control-plane": ""}
	if err := p.CheckNode("master-1", cp); err == nil {
		t.Fatal("nodo del control plane: atteso rifiuto")
	}
	if err := p.CheckNode("worker-1", map[string]string{"node-role.kubernetes.io/worker": ""}); err != nil {
		t.Fatal(err)
	}
	p.AllowControlPlaneNodes = true
	if err := p.CheckNode("master-1", cp); err != nil {
		t.Fatal(err)
	}
}
