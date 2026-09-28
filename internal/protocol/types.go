// Package protocol definisce i messaggi scambiati tra agente e backend
// (vedi la scheda "Protocollo agente–backend" del documento di progetto).
package protocol

import "time"

// Tipi di azione ammessi dal protocollo v1.
const (
	ActionRolloutRestart = "rollout-restart"
	ActionScale          = "scale"
	ActionCordon         = "cordon"
	ActionUncordon       = "uncordon"
	ActionDrain          = "drain"
)

// ActionTypes elenca tutti i tipi di azione noti.
var ActionTypes = []string{ActionRolloutRestart, ActionScale, ActionCordon, ActionUncordon, ActionDrain}

// IsNodeAction indica se l'azione agisce su un nodo invece che su un deployment.
func IsNodeAction(t string) bool {
	return t == ActionCordon || t == ActionUncordon || t == ActionDrain
}

// Esiti possibili di un'azione.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"
)

type RegisterRequest struct {
	CSR               string `json:"csr"`
	AgentVersion      string `json:"agent_version"`
	KubernetesVersion string `json:"kubernetes_version"`
	ClusterUID        string `json:"cluster_uid"`
}

type RegisterResponse struct {
	ClusterID   string    `json:"cluster_id"`
	Certificate string    `json:"certificate"`
	CAChain     string    `json:"ca_chain"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type CertificateRequest struct {
	CSR string `json:"csr"`
}

type CertificateResponse struct {
	Certificate string    `json:"certificate"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type NodeCount struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}

type HeartbeatRequest struct {
	AgentVersion      string          `json:"agent_version"`
	KubernetesVersion string          `json:"kubernetes_version"`
	Nodes             NodeCount       `json:"nodes"`
	Permissions       map[string]bool `json:"permissions"`
	LastActionID      string          `json:"last_action_id,omitempty"`
}

type PollConfig struct {
	IntervalSeconds int `json:"interval_seconds"`
	WaitSeconds     int `json:"wait_seconds"`
}

type HeartbeatResponse struct {
	ServerTime      time.Time  `json:"server_time"`
	Poll            PollConfig `json:"poll"`
	MinAgentVersion string     `json:"min_agent_version"`
}

// Params contiene i parametri già risolti dal backend a partire dal runbook
// e dalle scelte dell'utente nell'app.
type Params struct {
	Namespace          string `json:"namespace,omitempty"`
	Deployment         string `json:"deployment,omitempty"`
	Node               string `json:"node,omitempty"`
	Replicas           *int   `json:"replicas,omitempty"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty"`
	DeleteEmptyDirData bool   `json:"delete_emptydir_data,omitempty"`
}

type Action struct {
	ActionID    string    `json:"action_id"`
	Type        string    `json:"type"`
	Params      Params    `json:"params"`
	Runbook     string    `json:"runbook,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	ApprovedBy  []string  `json:"approved_by,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type ActionsResponse struct {
	Actions         []Action `json:"actions"`
	StatusRequested bool     `json:"status_requested,omitempty"`
}

type Result struct {
	Status     string         `json:"status"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
}
