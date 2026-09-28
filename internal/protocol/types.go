// Package protocol defines the messages exchanged between agent and backend
// (see protocol/protocol.md in the thumbops/spec repository).
package protocol

import "time"

// Action types allowed by protocol v1.
const (
	ActionRolloutRestart = "rollout-restart"
	ActionScale          = "scale"
	ActionCordon         = "cordon"
	ActionUncordon       = "uncordon"
	ActionDrain          = "drain"
)

// ActionTypes lists all known action types.
var ActionTypes = []string{ActionRolloutRestart, ActionScale, ActionCordon, ActionUncordon, ActionDrain}

// IsNodeAction reports whether the action targets a node instead of a deployment.
func IsNodeAction(t string) bool {
	return t == ActionCordon || t == ActionUncordon || t == ActionDrain
}

// Possible outcomes of an action.
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

// Params holds the parameters already resolved by the backend from the runbook
// and the user's choices in the app.
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

// ClusterStatus is the compact summary sent with PUT /v1/agent/status
// (components.schemas.ClusterStatus in the contract).
type ClusterStatus struct {
	CollectedAt time.Time      `json:"collected_at"`
	Resources   Resources      `json:"resources"`
	Nodes       NodesSummary   `json:"nodes"`
	Workloads   WorkloadsState `json:"workloads"`
	Truncated   bool           `json:"truncated"`
}

type Resources struct {
	CPU    CPUUsage    `json:"cpu"`
	Memory MemoryUsage `json:"memory"`
}

// Used fields are null without metrics-server; the agent does not read
// metrics yet, so they are always null.
type CPUUsage struct {
	AllocatableM int64  `json:"allocatable_m"`
	RequestedM   int64  `json:"requested_m"`
	UsedM        *int64 `json:"used_m"`
}

type MemoryUsage struct {
	AllocatableMiB int64  `json:"allocatable_mib"`
	RequestedMiB   int64  `json:"requested_mib"`
	UsedMiB        *int64 `json:"used_mib"`
}

type NodesSummary struct {
	Total    int          `json:"total"`
	Ready    int          `json:"ready"`
	Cordoned int          `json:"cordoned"`
	Items    []NodeStatus `json:"items"`
}

type NodeStatus struct {
	Name          string   `json:"name"`
	Ready         bool     `json:"ready"`
	Unschedulable bool     `json:"unschedulable"`
	Conditions    []string `json:"conditions"`
	CPU           struct {
		AllocatableM int64 `json:"allocatable_m"`
		RequestedM   int64 `json:"requested_m"`
	} `json:"cpu"`
	Memory struct {
		AllocatableMiB int64 `json:"allocatable_mib"`
		RequestedMiB   int64 `json:"requested_mib"`
	} `json:"memory"`
}

type WorkloadsState struct {
	UnhealthyPods       []UnhealthyPod       `json:"unhealthy_pods"`
	DegradedDeployments []DegradedDeployment `json:"degraded_deployments"`
}

type UnhealthyPod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
	Restarts  int32  `json:"restarts"`
}

type DegradedDeployment struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ready     int32  `json:"ready"`
	Desired   int32  `json:"desired"`
}
