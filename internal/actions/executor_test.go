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
	}, nil)
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
	first := e.Execute(context.Background(), a, nil)
	second := e.Execute(context.Background(), a, nil) // e.g. agent restarted before sending the result

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
	}, nil)
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
	}, nil)
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
	}, nil)
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
	}, nil)
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "permission denied") {
		t.Fatalf("unexpected message: %s", r.Message)
	}
}

func TestCordonUncordon(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)

	r := e.Execute(context.Background(), protocol.Action{ActionID: "c1", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}}, nil)
	expectStatus(t, r, protocol.StatusSucceeded)
	if !fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should be cordoned")
	}

	r = e.Execute(context.Background(), protocol.Action{ActionID: "u1", Type: protocol.ActionUncordon, Params: protocol.Params{Node: "worker-1"}}, nil)
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

	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}}, nil)
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

	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1"}}, nil)
	expectStatus(t, r, protocol.StatusFailed)
	if fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node should not have been cordoned")
	}
	if !fk.PodExists("payments", "cache-1") {
		t.Fatal("the pod should not have been touched")
	}

	// With explicit consent the drain proceeds.
	r = e.Execute(context.Background(), protocol.Action{ActionID: "d2", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", DeleteEmptyDirData: true}}, nil)
	expectStatus(t, r, protocol.StatusSucceeded)
}

func TestDrainBlockedByBarePod(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("default", "debug-shell", "worker-1", nil))

	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1"}}, nil)
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
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 1}}, nil)
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
	r := e.Execute(context.Background(), protocol.Action{ActionID: "x", Type: "delete-namespace"}, nil)
	expectStatus(t, r, protocol.StatusRejected)
}

type progressLog struct{ got []protocol.Progress }

func (l *progressLog) add(p protocol.Progress) { l.got = append(l.got, p) }

func inProgress(t *testing.T, n kube.Node) map[string]any {
	t.Helper()
	raw, ok := n.Metadata.Annotations[AnnotationDrainInProgress]
	if !ok {
		return nil
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("drain-in-progress annotation: %v", err)
	}
	return st
}

func TestDrainProgressAndAnnotation(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	fk.AddPod(pod("payments", "api-2", "worker-1", ctrl("ReplicaSet", "api-rs")))
	fk.BlockEviction("payments", "api-2", 3) // PDB: three rejections

	var log progressLog
	var atFirst kube.Node
	firstSeen := false
	var patchesAtFirst []string
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}}, func(p protocol.Progress) {
		if !firstSeen {
			firstSeen = true
			atFirst = fk.Node("worker-1")
			patchesAtFirst = fk.Patches()
		}
		log.add(p)
	})
	expectStatus(t, r, protocol.StatusSucceeded)

	// The cordon and the annotation arrive in one patch: at the first progress
	// report there is exactly one node patch and it did both.
	if len(patchesAtFirst) != 1 || !atFirst.Spec.Unschedulable || inProgress(t, atFirst)["action_id"] != "d1" {
		t.Fatalf("cordon and annotation must come in one patch: %v %+v", patchesAtFirst, atFirst.Metadata.Annotations)
	}
	if len(log.got) < 2 {
		t.Fatalf("expected several progress reports, got %d", len(log.got))
	}
	first := log.got[0]
	if first.Details["evicted"] != 0 || first.Details["remaining"] != 2 {
		t.Fatalf("first progress (right after the cordon): %+v", first.Details)
	}
	sawBlocked := false
	for _, p := range log.got {
		if b, _ := p.Details["blocked"].([]map[string]string); len(b) == 1 && b[0]["pod"] == "payments/api-2" && b[0]["reason"] == "PodDisruptionBudget" {
			sawBlocked = true
		}
	}
	if !sawBlocked {
		t.Fatalf("no progress reported the PDB-blocked pod: %+v", log.got)
	}
	prev := -1
	for _, p := range log.got {
		ev, _ := p.Details["evicted"].(int)
		if ev < prev {
			t.Fatalf("evicted must never decrease: %d after %d (%+v)", ev, prev, log.got)
		}
		prev = ev
	}
	if st := inProgress(t, fk.Node("worker-1")); st != nil {
		t.Fatalf("the in-progress annotation must be removed at the end: %v", st)
	}
	if fk.Node("worker-1").Metadata.Annotations[AnnotationLastActionID] != "d1" {
		t.Fatal("result annotation missing")
	}
}

func TestDrainTimeoutRemovesTheAnnotation(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "db-0", "worker-1", ctrl("StatefulSet", "db")))
	fk.BlockEviction("payments", "db-0", -1)
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 1}}, nil)
	expectStatus(t, r, protocol.StatusFailed)
	if st := inProgress(t, fk.Node("worker-1")); st != nil {
		t.Fatalf("annotation left after a timeout: %v", st)
	}
	if !fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node stays cordoned")
	}
}

func TestDrainStoppedByTheBackend(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "db-0", "worker-1", ctrl("StatefulSet", "db")))
	fk.BlockEviction("payments", "db-0", -1)
	ctx, cancel := context.WithCancelCause(context.Background())
	progress := func(protocol.Progress) { cancel(ErrActionGone) } // 410 on the first progress
	r := e.Execute(ctx, protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}}, progress)
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "stopped") {
		t.Fatalf("message: %s", r.Message)
	}
	n := fk.Node("worker-1")
	if inProgress(t, n) != nil || !n.Spec.Unschedulable {
		t.Fatalf("expected no annotation and a cordoned node: %+v", n.Metadata.Annotations)
	}
	if n.Metadata.Annotations[AnnotationLastActionID] == "d1" {
		t.Fatal("a stopped drain must not write a result annotation")
	}
	if len(fk.Evictions()) != 0 {
		t.Fatalf("no eviction after the stop: %v", fk.Evictions())
	}
}

func TestDrainShutdownKeepsTheAnnotation(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "db-0", "worker-1", ctrl("StatefulSet", "db")))
	fk.BlockEviction("payments", "db-0", -1)
	ctx, cancel := context.WithCancel(context.Background())
	progress := func(protocol.Progress) { cancel() } // agent shutting down
	e.Execute(ctx, protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}}, progress)
	st := inProgress(t, fk.Node("worker-1"))
	if st == nil || st["action_id"] != "d1" || st["timeout_seconds"] != float64(5) {
		t.Fatalf("annotation must survive a shutdown for the resume: %v", st)
	}
}

func TestDrainResume(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	started := time.Now().Add(-2 * time.Second).UTC()
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": started, "timeout_seconds": 60, "delete_emptydir_data": false})
	fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: string(state)}}})

	acts, dropped, err := e.InProgressDrains(context.Background())
	if err != nil || len(dropped) != 0 || len(acts) != 1 {
		t.Fatalf("in-progress drains: %v %v %v", acts, dropped, err)
	}
	a := acts[0]
	if a.ActionID != "d1" || a.Type != protocol.ActionDrain || a.Params.Node != "worker-1" || a.Params.TimeoutSeconds != 60 {
		t.Fatalf("rebuilt action: %+v", a)
	}
	r := e.Execute(context.Background(), a, nil)
	expectStatus(t, r, protocol.StatusSucceeded)
	if !r.StartedAt.Equal(started.Truncate(time.Second)) && !r.StartedAt.Equal(started) {
		t.Fatalf("a resumed drain keeps its original start: %s vs %s", r.StartedAt, started)
	}
	if fk.PodExists("payments", "api-1") || inProgress(t, fk.Node("worker-1")) != nil {
		t.Fatal("the resumed drain must evict and clear the annotation")
	}
}

func TestDrainResumeWithNoTimeLeft(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().Add(-time.Hour).UTC(), "timeout_seconds": 60})
	fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: string(state)}}})
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 60}}, nil)
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "timed out") || !strings.Contains(r.Message, "payments/api-1") {
		t.Fatalf("message: %s", r.Message)
	}
	if !fk.PodExists("payments", "api-1") || len(fk.Evictions()) != 0 {
		t.Fatal("no eviction with no time left")
	}
	if inProgress(t, fk.Node("worker-1")) != nil {
		t.Fatal("annotation must be cleared")
	}
}

func TestInProgressDrainsDropsUnreadableAnnotations(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.PatchNodeForTest("worker-1", map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: "{not json"}}})
	acts, dropped, err := e.InProgressDrains(context.Background())
	if err != nil || len(acts) != 0 || len(dropped) != 1 || dropped[0] != "worker-1" {
		t.Fatalf("got %v %v %v", acts, dropped, err)
	}
	if _, ok := fk.Node("worker-1").Metadata.Annotations[AnnotationDrainInProgress]; ok {
		t.Fatal("an unreadable annotation must be removed")
	}
}

func TestDrainSkipsTheAgentPod(t *testing.T) {
	fk, e := setup(t)
	e.SelfNamespace, e.SelfName = "thumbops", "thumbops-agent-abc"
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("thumbops", "thumbops-agent-abc", "worker-1", ctrl("ReplicaSet", "thumbops-agent-rs")))
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}}, nil)
	expectStatus(t, r, protocol.StatusSucceeded)
	if !fk.PodExists("thumbops", "thumbops-agent-abc") {
		t.Fatal("the agent must not evict itself")
	}
	if r.Details["agent_pod_left"] != "thumbops/thumbops-agent-abc" || !strings.Contains(r.Message, "agent") {
		t.Fatalf("result must report the agent pod: %s %+v", r.Message, r.Details)
	}
}

func TestDrainResumeReassertsTheCordon(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "api-1", "worker-1", ctrl("ReplicaSet", "api-rs")))
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().UTC(), "timeout_seconds": 60})
	fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": false},
		"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: string(state)}}})
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 60}}, nil)
	expectStatus(t, r, protocol.StatusSucceeded)
	if !fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("a resumed drain must cordon the node again")
	}
}

func TestDrainResumeWithABlocker(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	fk.AddPod(pod("payments", "bare", "worker-1", nil))
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().UTC(), "timeout_seconds": 60})
	fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: string(state)}}})
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 60}}, nil)
	expectStatus(t, r, protocol.StatusFailed)
	if !strings.Contains(r.Message, "aborted on resume") {
		t.Fatalf("message: %s", r.Message)
	}
	n := fk.Node("worker-1")
	if !n.Spec.Unschedulable || inProgress(t, n) != nil {
		t.Fatalf("expected a cordoned node with no annotation: %+v", n.Metadata.Annotations)
	}
}

func TestUncordonEndsADrainInProgress(t *testing.T) {
	fk, e := setup(t)
	fk.AddNode("worker-1", true, nil)
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().UTC(), "timeout_seconds": 60})
	fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: string(state)}}})
	r := e.Execute(context.Background(), protocol.Action{ActionID: "u1", Type: protocol.ActionUncordon,
		Params: protocol.Params{Node: "worker-1"}}, nil)
	expectStatus(t, r, protocol.StatusSucceeded)
	n := fk.Node("worker-1")
	if n.Spec.Unschedulable || inProgress(t, n) != nil {
		t.Fatalf("uncordon must clear the drain in progress: %+v", n)
	}
}
