package status

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func node(name string, ready bool, cpu, mem string, mods ...func(*corev1.Node)) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
		},
	}
	for _, m := range mods {
		m(n)
	}
	return n
}

func container(cpu, mem string) corev1.Container {
	return corev1.Container{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
	}}}
}

func pod(ns, name, nodeName string, phase corev1.PodPhase, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		Spec:       corev1.PodSpec{NodeName: nodeName, Containers: containers},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func waiting(p *corev1.Pod, reason string, restarts int32) *corev1.Pod {
	p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: "c", RestartCount: restarts, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	})
	return p
}

func deployment(ns, name string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &desired},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func TestResourcesAndNodes(t *testing.T) {
	nodes := []*corev1.Node{
		node("a", true, "4", "16Gi"),
		node("b", false, "2", "8Gi", func(n *corev1.Node) {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
		}),
		node("c", true, "2", "8Gi", func(n *corev1.Node) { n.Spec.Unschedulable = true }),
	}
	initPod := pod("web", "with-init", "a", corev1.PodRunning, container("250m", "256Mi"), container("250m", "256Mi"))
	initPod.Spec.InitContainers = []corev1.Container{container("1", "128Mi")} // max(init, sum) per resource
	withOverhead := pod("web", "overhead", "b", corev1.PodRunning, container("100m", "100Mi"))
	withOverhead.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("28Mi")}
	pods := []*corev1.Pod{
		initPod,
		withOverhead,
		pod("web", "done", "a", corev1.PodSucceeded, container("2", "2Gi")), // terminated: no longer counts
		pod("web", "unscheduled", "", corev1.PodPending, container("2", "2Gi")),
	}

	s := Summarize(nodes, pods, nil, Options{Now: now})

	if s.Resources.CPU.AllocatableM != 8000 || s.Resources.Memory.AllocatableMiB != 32*1024 {
		t.Fatalf("allocatable: %+v", s.Resources)
	}
	// with-init: cpu max(1000, 500) = 1000, memory max(128, 512) = 512; overhead pod: 150m, 128Mi.
	if s.Resources.CPU.RequestedM != 1150 || s.Resources.Memory.RequestedMiB != 640 {
		t.Fatalf("requested: %+v", s.Resources)
	}
	if s.Resources.CPU.UsedM != nil || s.Resources.Memory.UsedMiB != nil {
		t.Fatal("used must be null without metrics")
	}
	if s.Nodes.Total != 3 || s.Nodes.Ready != 2 || s.Nodes.Cordoned != 1 || len(s.Nodes.Items) != 3 {
		t.Fatalf("nodes: %+v", s.Nodes)
	}
	b := s.Nodes.Items[1]
	if b.Name != "b" || b.Ready || strings.Join(b.Conditions, ",") != "MemoryPressure" || b.CPU.RequestedM != 150 {
		t.Fatalf("node b: %+v", b)
	}
	if s.Nodes.Items[0].Conditions == nil {
		t.Fatal("conditions must be an empty list, not null")
	}
	if s.Truncated {
		t.Fatal("nothing was cut")
	}
}

func TestUnhealthyPodsBySeverity(t *testing.T) {
	pending := pod("shop", "pending-long", "", corev1.PodPending)
	pending.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}
	justCreated := pod("shop", "pending-new", "", corev1.PodPending)
	justCreated.CreationTimestamp = metav1.NewTime(now.Add(-30 * time.Second))
	failed := pod("batch", "job-x", "a", corev1.PodFailed)
	failed.Status.Reason = "Evicted"
	pods := []*corev1.Pod{
		pending,
		justCreated, // too young to be a problem
		waiting(pod("shop", "api-2", "a", corev1.PodRunning), "CrashLoopBackOff", 3),
		waiting(pod("shop", "api-1", "a", corev1.PodRunning), "CrashLoopBackOff", 14),
		waiting(pod("shop", "img", "a", corev1.PodPending), "ImagePullBackOff", 0),
		failed,
		pod("shop", "fine", "a", corev1.PodRunning),
	}

	s := Summarize(nil, pods, nil, Options{Now: now})

	var got []string
	for _, p := range s.Workloads.UnhealthyPods {
		got = append(got, fmt.Sprintf("%s/%s:%s:%d", p.Namespace, p.Name, p.Reason, p.Restarts))
	}
	want := []string{
		"shop/api-1:CrashLoopBackOff:14",
		"shop/api-2:CrashLoopBackOff:3",
		"shop/img:ImagePullBackOff:0",
		"batch/job-x:Evicted:0",
		"shop/pending-long:Unschedulable:0",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("unhealthy pods:\n got  %v\n want %v", got, want)
	}
}

// Between restarts a crash-looping container is terminated, not waiting:
// seen on kind, where it made crash-looping pods vanish from the summary.
func TestCrashingContainerBetweenRestarts(t *testing.T) {
	terminated := func(p *corev1.Pod, reason string, exitCode, restarts int32) *corev1.Pod {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: "c", RestartCount: restarts,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: exitCode}},
		})
		return p
	}
	pods := []*corev1.Pod{
		terminated(pod("shop", "exits", "a", corev1.PodRunning), "Error", 1, 4),
		terminated(pod("shop", "oom", "a", corev1.PodRunning), "OOMKilled", 137, 2),
		terminated(pod("shop", "finished-ok", "a", corev1.PodRunning), "Completed", 0, 0), // e.g. a sidecar that exited cleanly
	}
	s := Summarize(nil, pods, nil, Options{Now: now})
	var got []string
	for _, p := range s.Workloads.UnhealthyPods {
		got = append(got, fmt.Sprintf("%s:%s:%d", p.Name, p.Reason, p.Restarts))
	}
	if strings.Join(got, " ") != "exits:Error:4 oom:OOMKilled:2" {
		t.Fatalf("unhealthy pods: %v", got)
	}
}

func TestDegradedDeployments(t *testing.T) {
	deps := []*appsv1.Deployment{
		deployment("shop", "ok", 3, 3),
		deployment("shop", "half", 4, 2),
		deployment("shop", "down", 3, 0),
		deployment("shop", "scaled-to-zero", 0, 0),
	}
	s := Summarize(nil, nil, deps, Options{Now: now})
	d := s.Workloads.DegradedDeployments
	if len(d) != 2 || d[0].Name != "down" || d[1].Name != "half" || d[1].Ready != 2 || d[1].Desired != 4 {
		t.Fatalf("degraded: %+v", d)
	}
}

func TestExcludedNamespacesNeverListed(t *testing.T) {
	pods := []*corev1.Pod{
		waiting(pod("secret-team", "api", "a", corev1.PodRunning, container("500m", "1Gi")), "CrashLoopBackOff", 5),
		waiting(pod("shop", "api", "a", corev1.PodRunning), "CrashLoopBackOff", 1),
	}
	deps := []*appsv1.Deployment{deployment("secret-team", "api", 2, 0), deployment("shop", "api", 2, 1)}
	s := Summarize([]*corev1.Node{node("a", true, "4", "16Gi")}, pods, deps, Options{Now: now, ExcludeNamespaces: []string{"secret-team"}})

	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "secret-team") {
		t.Fatalf("an excluded namespace leaked: %s", raw)
	}
	// Aggregates still count its requests: they reveal no names.
	if s.Resources.CPU.RequestedM != 500 {
		t.Fatalf("requested: %+v", s.Resources.CPU)
	}
}

func TestSizeLimits(t *testing.T) {
	var nodes []*corev1.Node
	for i := range 120 {
		nodes = append(nodes, node(fmt.Sprintf("n%03d", i), i != 7, "1", "1Gi"))
	}
	var pods []*corev1.Pod
	var deps []*appsv1.Deployment
	for i := range 25 {
		pods = append(pods, waiting(pod("shop", fmt.Sprintf("p%02d", i), "n000", corev1.PodRunning), "CrashLoopBackOff", int32(i)))
		deps = append(deps, deployment("shop", fmt.Sprintf("d%02d", i), 2, 1))
	}
	s := Summarize(nodes, pods, deps, Options{Now: now})
	if s.Nodes.Total != 120 || len(s.Nodes.Items) != 1 || s.Nodes.Items[0].Name != "n007" {
		t.Fatalf("beyond 100 nodes only the ones with problems: total %d, items %d", s.Nodes.Total, len(s.Nodes.Items))
	}
	if len(s.Workloads.UnhealthyPods) != 20 || s.Workloads.UnhealthyPods[0].Restarts != 24 {
		t.Fatalf("pods: %d, first %+v", len(s.Workloads.UnhealthyPods), s.Workloads.UnhealthyPods[0])
	}
	if len(s.Workloads.DegradedDeployments) != 20 || !s.Truncated {
		t.Fatalf("deployments %d, truncated %v", len(s.Workloads.DegradedDeployments), s.Truncated)
	}
}

func TestEmptyClusterMarshalsLists(t *testing.T) {
	raw, _ := json.Marshal(Summarize(nil, nil, nil, Options{Now: now}))
	for _, want := range []string{`"items":[]`, `"unhealthy_pods":[]`, `"degraded_deployments":[]`, `"used_m":null`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}
