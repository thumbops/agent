package policy

import (
	"os"
	"strings"
	"testing"
)

// The sample policy in the manifest must load in the agent.
func TestManifestPolicyLoads(t *testing.T) {
	data, err := os.ReadFile("../../deploy/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	start := strings.Index(s, "policy.json: |")
	if start < 0 {
		t.Fatal("policy.json not found in the manifest")
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
		t.Fatalf("unexpected policy loaded: %+v", p)
	}
}
