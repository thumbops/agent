package status

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thumbops/agent/internal/protocol"
)

func TestCollectorFollowsTheCluster(t *testing.T) {
	cs := fake.NewClientset(
		node("a", true, "4", "16Gi"),
		waiting(pod("shop", "api", "a", corev1.PodRunning, container("500m", "1Gi")), "CrashLoopBackOff", 2),
		deployment("shop", "api", 2, 1),
	)
	c := NewCollector(cs, nil)
	if _, ok := c.Collect(now); ok {
		t.Fatal("no summary before the caches sync")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	s := waitFor(t, c, func(s protocol.ClusterStatus) bool { return s.Nodes.Total == 1 })
	if s.Resources.CPU.RequestedM != 500 || len(s.Workloads.UnhealthyPods) != 1 || len(s.Workloads.DegradedDeployments) != 1 {
		t.Fatalf("summary: %+v", s)
	}

	// Changes arrive through the watch, without listing again.
	if _, err := cs.CoreV1().Nodes().Create(ctx, node("b", false, "2", "8Gi"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s = waitFor(t, c, func(s protocol.ClusterStatus) bool { return s.Nodes.Total == 2 })
	if s.Nodes.Ready != 1 {
		t.Fatalf("nodes: %+v", s.Nodes)
	}
}

func waitFor(t *testing.T, c *Collector, done func(protocol.ClusterStatus) bool) protocol.ClusterStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := c.Collect(now); ok && done(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the collector did not catch up")
	return protocol.ClusterStatus{}
}
