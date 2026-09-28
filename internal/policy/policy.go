// Package policy implements the cluster's local policy: the second line of
// defense, managed by the cluster team rather than the backend. An action
// outside the policy is rejected even if the backend approved it.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/thumbops/agent/internal/protocol"
)

// Policy is read from a ConfigMap mounted as a JSON file.
// Without a file, the empty policy rejects every action.
type Policy struct {
	AllowedActions          []string     `json:"allowed_actions"`
	AllowedNamespaces       []string     `json:"allowed_namespaces,omitempty"` // empty = all except the denied ones
	DeniedNamespaces        []string     `json:"denied_namespaces,omitempty"`
	MaxReplicas             int          `json:"max_replicas,omitempty"` // 0 = no limit
	AllowDeleteEmptyDirData bool         `json:"allow_delete_emptydir_data,omitempty"`
	AllowControlPlaneNodes  bool         `json:"allow_control_plane_nodes,omitempty"`
	Status                  StatusPolicy `json:"status,omitempty"`
}

type StatusPolicy struct {
	ExcludeNamespaces []string `json:"exclude_namespaces,omitempty"`
}

// Violation is the reason an action is rejected.
type Violation struct{ Reason string }

func (v *Violation) Error() string { return v.Reason }

func reject(format string, args ...any) error {
	return &Violation{Reason: fmt.Sprintf(format, args...)}
}

// Load reads the policy. An empty path or a missing file give the empty
// policy (deny everything); a malformed file is an error, so the agent never
// starts with rules different from the ones the team thinks it wrote.
func Load(path string) (*Policy, error) {
	if path == "" {
		return &Policy{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Policy{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid local policy: %w", err)
	}
	for _, a := range p.AllowedActions {
		if !slices.Contains(protocol.ActionTypes, a) {
			return nil, fmt.Errorf("invalid local policy: unknown action type %q", a)
		}
	}
	if p.MaxReplicas < 0 {
		return nil, errors.New("invalid local policy: negative max_replicas")
	}
	return &p, nil
}

// Check validates an action before it runs.
func (p *Policy) Check(a protocol.Action) error {
	if !slices.Contains(p.AllowedActions, a.Type) {
		return reject("action %q is not allowed by the cluster's local policy", a.Type)
	}
	if protocol.IsNodeAction(a.Type) {
		if a.Params.Node == "" {
			return reject("missing node parameter")
		}
		if a.Type == protocol.ActionDrain && a.Params.DeleteEmptyDirData && !p.AllowDeleteEmptyDirData {
			return reject("the local policy does not allow deleting emptyDir volume data during a drain")
		}
		return nil
	}
	ns := a.Params.Namespace
	if ns == "" || a.Params.Deployment == "" {
		return reject("namespace and deployment parameters are required")
	}
	if slices.Contains(p.DeniedNamespaces, ns) {
		return reject("namespace %q is denied by the local policy", ns)
	}
	if len(p.AllowedNamespaces) > 0 && !slices.Contains(p.AllowedNamespaces, ns) {
		return reject("namespace %q is not among those allowed by the local policy", ns)
	}
	if a.Type == protocol.ActionScale {
		if a.Params.Replicas == nil || *a.Params.Replicas < 0 {
			return reject("replica count missing or negative")
		}
		if p.MaxReplicas > 0 && *a.Params.Replicas > p.MaxReplicas {
			return reject("%d replicas exceed the local policy maximum (%d)", *a.Params.Replicas, p.MaxReplicas)
		}
	}
	return nil
}

var controlPlaneLabels = []string{"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"}

// CheckNode protects control plane nodes, unless explicitly allowed.
func (p *Policy) CheckNode(name string, labels map[string]string) error {
	if p.AllowControlPlaneNodes {
		return nil
	}
	for _, l := range controlPlaneLabels {
		if _, ok := labels[l]; ok {
			return reject("node %s is part of the control plane: the local policy does not allow acting on these nodes", name)
		}
	}
	return nil
}
