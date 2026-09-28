// Package status builds the compact cluster summary the agent sends with
// PUT /v1/agent/status (spec: "Cluster status"). Summarize is a pure
// function over nodes, pods and deployments; Collector feeds it from
// client-go informers.
package status

import (
	"cmp"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/thumbops/agent/internal/protocol"
)

// Size limits from the protocol.
const (
	MaxNodeItems       = 100 // beyond this, only nodes with problems
	MaxUnhealthyPods   = 20
	MaxDegradedDeploys = 20
	// A pod pending for less than this is still being scheduled or started.
	PendingGrace = 2 * time.Minute
)

type Options struct {
	// Collection time; zero means time.Now().
	Now time.Time
	// Namespaces whose pods and deployments are never listed (local policy
	// status.exclude_namespaces). Their requests still count in the
	// aggregates, which reveal no names.
	ExcludeNamespaces []string
}

// problemConditions are the node conditions reported when True.
var problemConditions = []corev1.NodeConditionType{
	corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable,
}

// Summarize builds the status summary. Inputs are not modified.
func Summarize(nodes []*corev1.Node, pods []*corev1.Pod, deployments []*appsv1.Deployment, opts Options) protocol.ClusterStatus {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	excluded := make(map[string]bool, len(opts.ExcludeNamespaces))
	for _, ns := range opts.ExcludeNamespaces {
		excluded[ns] = true
	}

	s := protocol.ClusterStatus{CollectedAt: now.UTC()}
	s.Nodes.Items = []protocol.NodeStatus{}
	s.Workloads.UnhealthyPods = []protocol.UnhealthyPod{}
	s.Workloads.DegradedDeployments = []protocol.DegradedDeployment{}

	// Requests of the pods that hold resources on a node.
	type req struct{ cpuM, memBytes int64 }
	perNode := map[string]req{}
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem := podRequests(p)
		r := perNode[p.Spec.NodeName]
		perNode[p.Spec.NodeName] = req{r.cpuM + cpu, r.memBytes + mem}
	}

	var items, problems []protocol.NodeStatus
	var allocCPU, allocMem, reqCPU, reqMem int64
	for _, n := range sortedByName(nodes) {
		ns := protocol.NodeStatus{Name: n.Name, Unschedulable: n.Spec.Unschedulable, Conditions: []string{}}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ns.Ready = c.Status == corev1.ConditionTrue
			} else if c.Status == corev1.ConditionTrue && slices.Contains(problemConditions, c.Type) {
				ns.Conditions = append(ns.Conditions, string(c.Type))
			}
		}
		ns.CPU.AllocatableM = n.Status.Allocatable.Cpu().MilliValue()
		ns.Memory.AllocatableMiB = mib(n.Status.Allocatable.Memory().Value())
		r := perNode[n.Name]
		ns.CPU.RequestedM = r.cpuM
		ns.Memory.RequestedMiB = mib(r.memBytes)

		allocCPU += ns.CPU.AllocatableM
		allocMem += n.Status.Allocatable.Memory().Value()
		reqCPU += r.cpuM
		reqMem += r.memBytes
		s.Nodes.Total++
		if ns.Ready {
			s.Nodes.Ready++
		}
		if ns.Unschedulable {
			s.Nodes.Cordoned++
		}
		items = append(items, ns)
		if !ns.Ready || ns.Unschedulable || len(ns.Conditions) > 0 {
			problems = append(problems, ns)
		}
	}
	if len(items) > MaxNodeItems {
		items = problems
		s.Truncated = true
	}
	if items != nil {
		s.Nodes.Items = items
	}
	s.Resources.CPU = protocol.CPUUsage{AllocatableM: allocCPU, RequestedM: reqCPU}
	s.Resources.Memory = protocol.MemoryUsage{AllocatableMiB: mib(allocMem), RequestedMiB: mib(reqMem)}

	type ranked struct {
		protocol.UnhealthyPod
		severity int
	}
	var unhealthy []ranked
	for _, p := range pods {
		if excluded[p.Namespace] {
			continue
		}
		if reason, sev, ok := podProblem(p, now); ok {
			unhealthy = append(unhealthy, ranked{protocol.UnhealthyPod{Namespace: p.Namespace, Name: p.Name, Reason: reason, Restarts: restarts(p)}, sev})
		}
	}
	slices.SortFunc(unhealthy, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(a.severity, b.severity), cmp.Compare(b.Restarts, a.Restarts),
			cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	if len(unhealthy) > MaxUnhealthyPods {
		unhealthy = unhealthy[:MaxUnhealthyPods]
		s.Truncated = true
	}
	for _, u := range unhealthy {
		s.Workloads.UnhealthyPods = append(s.Workloads.UnhealthyPods, u.UnhealthyPod)
	}

	for _, d := range deployments {
		if excluded[d.Namespace] {
			continue
		}
		desired := int32(1) // Kubernetes default when replicas is unset
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		if desired > 0 && d.Status.ReadyReplicas < desired {
			s.Workloads.DegradedDeployments = append(s.Workloads.DegradedDeployments, protocol.DegradedDeployment{
				Namespace: d.Namespace, Name: d.Name, Ready: d.Status.ReadyReplicas, Desired: desired,
			})
		}
	}
	slices.SortFunc(s.Workloads.DegradedDeployments, func(a, b protocol.DegradedDeployment) int {
		return cmp.Or(cmp.Compare(b.Desired-b.Ready, a.Desired-a.Ready), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	if len(s.Workloads.DegradedDeployments) > MaxDegradedDeploys {
		s.Workloads.DegradedDeployments = s.Workloads.DegradedDeployments[:MaxDegradedDeploys]
		s.Truncated = true
	}
	return s
}

// podRequests follows the scheduler: the larger of the containers' sum and
// any single init container, per resource, plus the pod overhead.
func podRequests(p *corev1.Pod) (cpuM, memBytes int64) {
	for _, c := range p.Spec.Containers {
		cpuM += c.Resources.Requests.Cpu().MilliValue()
		memBytes += c.Resources.Requests.Memory().Value()
	}
	for _, c := range p.Spec.InitContainers {
		cpuM = max(cpuM, c.Resources.Requests.Cpu().MilliValue())
		memBytes = max(memBytes, c.Resources.Requests.Memory().Value())
	}
	if o, ok := p.Spec.Overhead[corev1.ResourceCPU]; ok {
		cpuM += o.MilliValue()
	}
	if o, ok := p.Spec.Overhead[corev1.ResourceMemory]; ok {
		memBytes += o.Value()
	}
	return cpuM, memBytes
}

// Severity: lower is worse.
const (
	sevCrashLoop = iota
	sevCannotStart
	sevFailed
	sevPending
)

var cannotStart = []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError"}

// podProblem says whether a pod is unhealthy, why, and how badly.
func podProblem(p *corev1.Pod, now time.Time) (reason string, severity int, ok bool) {
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, c := range statuses {
		if w := c.State.Waiting; w != nil {
			switch {
			case w.Reason == "CrashLoopBackOff":
				return w.Reason, sevCrashLoop, true
			case slices.Contains(cannotStart, w.Reason):
				return w.Reason, sevCannotStart, true
			}
		}
		// A crash-looping container spends part of its time terminated
		// between restarts, not waiting; a failed exit counts the same.
		if t := c.State.Terminated; t != nil && t.ExitCode != 0 && p.Status.Phase != corev1.PodFailed {
			return cmp.Or(t.Reason, "Error"), sevCrashLoop, true
		}
	}
	switch p.Status.Phase {
	case corev1.PodFailed:
		return cmp.Or(p.Status.Reason, "Failed"), sevFailed, true
	case corev1.PodPending:
		if now.Sub(p.CreationTimestamp.Time) < PendingGrace {
			return "", 0, false
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
				return c.Reason, sevPending, true
			}
		}
		return "Pending", sevPending, true
	}
	return "", 0, false
}

func restarts(p *corev1.Pod) int32 {
	var n int32
	for _, c := range p.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}

func mib(bytes int64) int64 { return bytes / (1024 * 1024) }

func sortedByName(nodes []*corev1.Node) []*corev1.Node {
	out := slices.Clone(nodes)
	slices.SortFunc(out, func(a, b *corev1.Node) int { return cmp.Compare(a.Name, b.Name) })
	return out
}
