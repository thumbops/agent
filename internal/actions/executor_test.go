package actions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/kubefake"
	"github.com/thumbops/agent/internal/protocol"
)

func setup(t *testing.T) (*kubefake.Server, *Executor) {
	t.Helper()
	fk := kubefake.New()
	t.Cleanup(fk.Close)
	e := New(fk.Client())
	e.PollInterval = 10 * time.Millisecond
	return fk, e
}

func intp(v int) *int { return &v }

func ctrl(kind, name string) []kube.OwnerReference {
	yes := true
	return []kube.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

func pod(ns, name, node string, owners []kube.OwnerReference) kube.Pod {
	return kube.Pod{
		Metadata: kube.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: owners},
		Spec:     kube.PodSpec{NodeName: node},
		Status:   kube.PodStatus{Phase: "Running"},
	}
}

func expectStatus(t *testing.T, r protocol.Result, want string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status %q, expected %q (message: %s)", r.Status, want, r.Message)
	}
}

func TestRolloutRestart(t *testing.T) {
	fk, e := setup(t)
	fk.AddDeployment("payments", "payments-api", 3)

	r := e.Execute(context.Background(), protocol.Action{
		ActionID: "a1", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api"},
	})
	expectStatus(t, r, protocol.StatusSucceeded)

	d := fk.Deployment("payments", "payments-api")
	if d.Spec.Template.Metadata.Annotations[annotationRestartedAt] == "" {
		t.Fatal("restartedAt annotation missing on the template")
	}
	if got := d.Metadata.Annotations[AnnotationLastActionID]; got != "a1" {
		t.Fatalf("last-action-id annotation = %q", got)
	}
	var stored protocol.Result
	if err := json.Unmarshal([]byte(d.Metadata.Annotations[AnnotationLastResult]), &stored); err != nil || stored.Status != protocol.StatusSucceeded {
		t.Fatalf("invalid result stored in the annotation: %v %+v", err, stored)
	}
	if r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		t.Fatalf("invalid result times: %v %v", r.StartedAt, r.FinishedAt)
	}
}

func TestIdempotentReplay(t *testing.T) {
	fk, e := setup(t)
	fk.AddDeployment("payments", "payments-api", 3)
	a := protocol.Action{
		ActionID: "a1", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api"},
	}
	first := e.Execute(context.Background(), a)
	second := e.Execute(context.Background(), a) // e.g. agent restarted before sending the result

	expectStatus(t, second, protocol.StatusSucceeded)
	if got := len(fk.Patches()); got != 1 {
		t.Fatalf("expected 1 patch, found %d: the action was repeated", got)
	}
	if second.Details["replayed"] != true {
		t.Fatal("the second result should be marked as replayed")
	}
	if second.Message != first.Message || !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("the resent result differs from the original: %+v vs %+v", second, first)
	}
}

func TestScale(t *testing.T) {
	fk, e := setup(t)
	fk.AddDeployment("payments", "payments-api", 3)

	r := e.Execute(context.Background(), protocol.Action{
		ActionID: "s1", Type: protocol.ActionScale,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api", Replicas: intp(6)},
	})
	expectStatus(t, r, protocol.StatusSucceeded)
	if got := *fk.Deployment("payments", "payments-api").Spec.Replicas; got != 6 {
		t.Fatalf("replicas = %d, expected 6", got)
	}
	if r.Details["previous_replicas"] != 3 || r.Details["replicas"] != 6 {
		t.Fatalf("invalid details: %+v", r.Details)
	}
	if r.Message != "payments/payments-api scaled from 3 to 6 replicas" {
		t.Fatalf("unexpected message: %s", r.Message)
	}
}

func TestScaleMissingReplicas(t *testing.T) {
	fk, e := setup(t)
	fk.AddDeployment("payments", "payments-api", 3)
	r := e.Execute(context.Background(), protocol.Action{
		ActionID: "s1", Type: protocol.ActionScale,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api"},
	})
	expectStatus(t, r, protocol.StatusFailed)
	if len(fk.Patches()) != 0 {
		t.Fatal("no change expected")
	}
}

func TestDeploymentNotFound(t *testing.T) {
	_, e := setup(t)
	r := e.Execute(context.Background(), protocol.Action{
		ActionID: "a1", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "payments", Deployment: "missing"},
	})
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "not found") {
		t.Fatalf("unexpected message: %s", r.Message)
	}
}

func TestForbidden(t *testing.T) {
	fk, e := setup(t)
	fk.AddDeployment("payments", "payments-api", 3)
	fk.Deny("patch", "apps", "deployments", "")
	r := e.Execute(context.Background(), protocol.Action{
		ActionID: "a1", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api"},
	})
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "permission denied") {
		t.Fatalf("unexpected message: %s", r.Message)
	}
}

func TestCordonUncordon(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)

	r := e.Execute(context.Background(), protocol.Action{ActionID: "c1", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	expectStatus(t, r, protocol.StatusSucceeded)
	if !fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should be cordoned")
	}

	r = e.Execute(context.Background(), protocol.Action{ActionID: "u1", Type: protocol.ActionUncordon, Params: protocol.Params{Node: "worker-1"}})
	expectStatus(t, r, protocol.StatusSucceeded)
	if fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should be schedulable again")
	}
	if r.Details["previously_unschedulable"] != true {
		t.Fatalf("invalid details: %+v", r.Details)
	}
}

func TestDrain(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddNode("worker-2", true, nil)
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	fk.AddPod(pod("payments", "api-2", "worker-1", ctrl("ReplicaSet", "api-rs")))
	fk.AddPod(pod("kube-system", "fluent-bit-x", "worker-1", ctrl("DaemonSet", "fluent-bit")))
	static := pod("kube-system", "kube-proxy-worker-1", "worker-1", nil)
	static.Metadata.Annotations = map[string]string{annotationMirrorPod: "abc"}
	fk.AddPod(static)
	done := pod("batch", "job-1-xyz", "worker-1", ctrl("Job", "job-1"))
	done.Status.Phase = "Succeeded"
	fk.AddPod(done)
	fk.AddPod(pod("payments", "api-3", "worker-2", ctrl("ReplicaSet", "api-rs"))) // other node
	fk.BlockEviction("payments", "api-2", 2)                                      // PDB: two rejections, then ok

	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}})
	expectStatus(t, r, protocol.StatusSucceeded)

	if !fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should be cordoned")
	}
	for _, p := range []string{"api-1", "api-2"} {
		if fk.PodExists("payments", p) {
			t.Fatalf("pod %s should have been evicted", p)
		}
	}
	for _, p := range [][2]string{{"kube-system", "fluent-bit-x"}, {"kube-system", "kube-proxy-worker-1"}, {"batch", "job-1-xyz"}, {"payments", "api-3"}} {
		if !fk.PodExists(p[0], p[1]) {
			t.Fatalf("pod %s/%s should not have been touched", p[0], p[1])
		}
	}
	if got := len(fk.Evictions()); got != 2 {
		t.Fatalf("expected 2 evictions, found %d: %v", got, fk.Evictions())
	}
	if fk.Node("worker-1").Metadata.Annotations[AnnotationLastActionID] != "d1" {
		t.Fatal("action annotation missing on the node")
	}
	if !strings.Contains(r.Message, "2 pods evicted, 3 skipped") {
		t.Fatalf("unexpected message: %s", r.Message)
	}
}

func TestDrainBlockedByEmptyDir(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	p := pod("payments", "cache-1", "worker-1", ctrl("ReplicaSet", "cache-rs"))
	p.Spec.Volumes = []kube.Volume{{Name: "tmp", EmptyDir: json.RawMessage(`{}`)}}
	fk.AddPod(p)

	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1"}})
	expectStatus(t, r, protocol.StatusFailed)
	if fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should not have been cordoned")
	}
	if !fk.PodExists("payments", "cache-1") {
		t.Fatal("the pod should not have been touched")
	}

	// With explicit consent the drain proceeds.
	r = e.Execute(context.Background(), protocol.Action{ActionID: "d2", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", DeleteEmptyDirData: true}})
	expectStatus(t, r, protocol.StatusSucceeded)
}

func TestDrainBlockedByBarePod(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("default", "debug-shell", "worker-1", nil))

	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1"}})
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "default/debug-shell is not managed by a controller") {
		t.Fatalf("unexpected message: %s", r.Message)
	}
	if fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should not have been cordoned")
	}
}

func TestDrainTimeout(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	fk.AddPod(pod("payments", "db-0", "worker-1", ctrl("StatefulSet", "db")))
	fk.BlockEviction("payments", "db-0", -1) // PDB that never unblocks

	start := time.Now()
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 1}})
	expectStatus(t, r, protocol.StatusFailed)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the timeout was not honored: %s", time.Since(start))
	}
	if !strings.Contains(r.Message, "payments/db-0") || !strings.Contains(r.Message, "stays cordoned") {
		t.Fatalf("unexpected message: %s", r.Message)
	}
	if fk.PodExists("payments", "api-1") {
		t.Fatal("the unblocked pod should still have been evicted")
	}
	if !fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node must stay cordoned after a partial drain")
	}
}

func TestUnknownAction(t *testing.T) {
	_, e := setup(t)
	r := e.Execute(context.Background(), protocol.Action{ActionID: "x", Type: "delete-namespace"})
	expectStatus(t, r, protocol.StatusRejected)
}
