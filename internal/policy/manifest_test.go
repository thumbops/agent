package policy

import (
	"os"
	"strings"
	"testing"
)

// La policy di esempio nel manifest deve essere caricabile dall'agente.
func TestManifestPolicyLoads(t *testing.T) {
	data, err := os.ReadFile("../../deploy/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	start := strings.Index(s, "policy.json: |")
	if start < 0 {
		t.Fatal("policy.json non trovata nel manifest")
	}
	var lines []string
	for _, l := range strings.Split(s[start:], "\n")[1:] {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(l, "    "))
	}
	p, err := Load(write(t, strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.AllowedActions) != 5 || p.MaxReplicas != 20 {
		t.Fatalf("policy caricata in modo inatteso: %+v", p)
	}
}
