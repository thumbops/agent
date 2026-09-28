// Package policy implementa la policy locale del cluster: il secondo livello
// di difesa, gestito dal team del cluster e non dal backend. Un'azione fuori
// policy viene rifiutata anche se il backend l'ha approvata.
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

// Policy viene letta da un ConfigMap montato come file JSON.
// Senza file, la policy vuota rifiuta qualsiasi azione.
type Policy struct {
	AllowedActions          []string     `json:"allowed_actions"`
	AllowedNamespaces       []string     `json:"allowed_namespaces,omitempty"` // vuoto = tutti tranne i negati
	DeniedNamespaces        []string     `json:"denied_namespaces,omitempty"`
	MaxReplicas             int          `json:"max_replicas,omitempty"` // 0 = nessun limite
	AllowDeleteEmptyDirData bool         `json:"allow_delete_emptydir_data,omitempty"`
	AllowControlPlaneNodes  bool         `json:"allow_control_plane_nodes,omitempty"`
	Status                  StatusPolicy `json:"status,omitempty"`
}

type StatusPolicy struct {
	ExcludeNamespaces []string `json:"exclude_namespaces,omitempty"`
}

// Violation è il motivo per cui un'azione viene rifiutata.
type Violation struct{ Reason string }

func (v *Violation) Error() string { return v.Reason }

func reject(format string, args ...any) error {
	return &Violation{Reason: fmt.Sprintf(format, args...)}
}

// Load legge la policy. Un percorso vuoto o un file assente danno la policy
// vuota (nega tutto); un file malformato è un errore, per non partire con
// regole diverse da quelle che il team crede di aver scritto.
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
		return nil, fmt.Errorf("policy locale non valida: %w", err)
	}
	for _, a := range p.AllowedActions {
		if !slices.Contains(protocol.ActionTypes, a) {
			return nil, fmt.Errorf("policy locale non valida: tipo di azione sconosciuto %q", a)
		}
	}
	if p.MaxReplicas < 0 {
		return nil, errors.New("policy locale non valida: max_replicas negativo")
	}
	return &p, nil
}

// Check controlla un'azione prima dell'esecuzione.
func (p *Policy) Check(a protocol.Action) error {
	if !slices.Contains(p.AllowedActions, a.Type) {
		return reject("azione %q non ammessa dalla policy locale del cluster", a.Type)
	}
	if protocol.IsNodeAction(a.Type) {
		if a.Params.Node == "" {
			return reject("parametro node mancante")
		}
		if a.Type == protocol.ActionDrain && a.Params.DeleteEmptyDirData && !p.AllowDeleteEmptyDirData {
			return reject("la policy locale non consente di cancellare i dati dei volumi emptyDir durante il drain")
		}
		return nil
	}
	ns := a.Params.Namespace
	if ns == "" || a.Params.Deployment == "" {
		return reject("parametri namespace e deployment obbligatori")
	}
	if slices.Contains(p.DeniedNamespaces, ns) {
		return reject("namespace %q escluso dalla policy locale", ns)
	}
	if len(p.AllowedNamespaces) > 0 && !slices.Contains(p.AllowedNamespaces, ns) {
		return reject("namespace %q non tra quelli ammessi dalla policy locale", ns)
	}
	if a.Type == protocol.ActionScale {
		if a.Params.Replicas == nil || *a.Params.Replicas < 0 {
			return reject("numero di repliche mancante o negativo")
		}
		if p.MaxReplicas > 0 && *a.Params.Replicas > p.MaxReplicas {
			return reject("%d repliche superano il massimo della policy locale (%d)", *a.Params.Replicas, p.MaxReplicas)
		}
	}
	return nil
}

var controlPlaneLabels = []string{"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"}

// CheckNode protegge i nodi del control plane, salvo consenso esplicito.
func (p *Policy) CheckNode(name string, labels map[string]string) error {
	if p.AllowControlPlaneNodes {
		return nil
	}
	for _, l := range controlPlaneLabels {
		if _, ok := labels[l]; ok {
			return reject("il nodo %s fa parte del control plane: la policy locale non consente di agire su questi nodi", name)
		}
	}
	return nil
}
