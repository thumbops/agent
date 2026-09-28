package kube

import "encoding/json"

// Only the fields the agent uses: the rest of each object is ignored.

type ObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []OwnerReference  `json:"ownerReferences,omitempty"`
}

type OwnerReference struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller *bool  `json:"controller,omitempty"`
}

type Deployment struct {
	Metadata ObjectMeta       `json:"metadata"`
	Spec     DeploymentSpec   `json:"spec"`
	Status   DeploymentStatus `json:"status"`
}

type DeploymentSpec struct {
	Replicas *int32           `json:"replicas,omitempty"`
	Template *PodTemplateSpec `json:"template,omitempty"`
}

type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata"`
}

type DeploymentStatus struct {
	Replicas          int32 `json:"replicas,omitempty"`
	ReadyReplicas     int32 `json:"readyReplicas,omitempty"`
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
}

type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     NodeSpec   `json:"spec"`
	Status   NodeStatus `json:"status"`
}

type NodeSpec struct {
	Unschedulable bool `json:"unschedulable,omitempty"`
}

type NodeStatus struct {
	Conditions []NodeCondition `json:"conditions,omitempty"`
}

type NodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Ready reports whether the node has the condition Ready=True.
func (n *Node) Ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status"`
}

type PodSpec struct {
	NodeName string   `json:"nodeName,omitempty"`
	Volumes  []Volume `json:"volumes,omitempty"`
}

type Volume struct {
	Name     string          `json:"name"`
	EmptyDir json.RawMessage `json:"emptyDir,omitempty"`
}

type PodStatus struct {
	Phase string `json:"phase,omitempty"`
}

// ControllerRef returns the reference to the pod's controller, if any.
func (p *Pod) ControllerRef() *OwnerReference {
	for i, o := range p.Metadata.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return &p.Metadata.OwnerReferences[i]
		}
	}
	return nil
}
