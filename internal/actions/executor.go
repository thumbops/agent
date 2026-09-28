// Package actions runs the protocol actions on the cluster.
//
// Every action is idempotent: the agent annotates the modified resource with
// the action_id and the result, in the same patch that applies the change. If
// the agent restarts between execution and sending the result, on the next
// delivery it finds the annotation and resends the result instead of
// repeating the action.
package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/protocol"
)

const (
	AnnotationLastActionID = "thumbops.mobiletechnologies.cloud/last-action-id"
	AnnotationLastResult   = "thumbops.mobiletechnologies.cloud/last-action-result"

	annotationRestartedAt = "kubectl.kubernetes.io/restartedAt"
	annotationMirrorPod   = "kubernetes.io/config.mirror"

	defaultDrainTimeout = 10 * time.Minute
)

type Executor struct {
	Kube         *kube.Client
	Now          func() time.Time
	PollInterval time.Duration // wait between attempts during a drain
}

func New(k *kube.Client) *Executor {
	return &Executor{Kube: k, Now: time.Now, PollInterval: 2 * time.Second}
}

// Execute runs the action and always returns a result, never an error:
// a failure is also a result to report to the backend.
func (e *Executor) Execute(ctx context.Context, a protocol.Action) protocol.Result {
	start := e.Now().UTC()
	var res protocol.Result
	switch a.Type {
	case protocol.ActionRolloutRestart:
		res = e.rolloutRestart(ctx, a, start)
	case protocol.ActionScale:
		res = e.scale(ctx, a, start)
	case protocol.ActionCordon, protocol.ActionUncordon:
		res = e.setSchedulable(ctx, a, start)
	case protocol.ActionDrain:
		res = e.drain(ctx, a, start)
	default:
		res = protocol.Result{Status: protocol.StatusRejected, Message: fmt.Sprintf("unknown action type %q", a.Type)}
	}
	if res.StartedAt.IsZero() {
		res.StartedAt = start
	}
	if res.FinishedAt.IsZero() {
		res.FinishedAt = e.Now().UTC()
	}
	return res
}

func (e *Executor) failed(start time.Time, format string, args ...any) protocol.Result {
	return protocol.Result{
		Status:     protocol.StatusFailed,
		StartedAt:  start,
		FinishedAt: e.Now().UTC(),
		Message:    fmt.Sprintf(format, args...),
	}
}

func (e *Executor) succeeded(start time.Time, details map[string]any, format string, args ...any) protocol.Result {
	return protocol.Result{
		Status:     protocol.StatusSucceeded,
		StartedAt:  start,
		FinishedAt: e.Now().UTC(),
		Message:    fmt.Sprintf(format, args...),
		Details:    details,
	}
}

// describe turns an API error into a message for the on-call engineer.
func describe(err error, what string) string {
	switch {
	case kube.IsNotFound(err):
		return what + " not found"
	case kube.IsForbidden(err):
		return "permission denied on " + what + ": check the agent's ClusterRole"
	default:
		return "Kubernetes API error on " + what + ": " + err.Error()
	}
}

// previousResult recognizes an action that was already executed.
func previousResult(ann map[string]string, actionID string) (protocol.Result, bool) {
	if actionID == "" || ann[AnnotationLastActionID] != actionID {
		return protocol.Result{}, false
	}
	var r protocol.Result
	if err := json.Unmarshal([]byte(ann[AnnotationLastResult]), &r); err != nil || r.Status == "" {
		r = protocol.Result{Status: protocol.StatusSucceeded, Message: "action already executed earlier"}
	}
	if r.Details == nil {
		r.Details = map[string]any{}
	}
	r.Details["replayed"] = true
	return r, true
}

func resultAnnotations(actionID string, r protocol.Result) map[string]any {
	b, _ := json.Marshal(r)
	return map[string]any{AnnotationLastActionID: actionID, AnnotationLastResult: string(b)}
}

func (e *Executor) rolloutRestart(ctx context.Context, a protocol.Action, start time.Time) protocol.Result {
	ns, name := a.Params.Namespace, a.Params.Deployment
	if ns == "" || name == "" {
		return e.failed(start, "namespace and deployment parameters are required")
	}
	what := fmt.Sprintf("deployment %s/%s", ns, name)
	d, err := e.Kube.GetDeployment(ctx, ns, name)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(d.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	now := e.Now().UTC().Format(time.RFC3339)
	res := e.succeeded(start, map[string]any{"restarted_at": now}, "rollout of %s/%s started", ns, name)
	// Same mechanism as "kubectl rollout restart": changing an annotation
	// on the template starts a new rollout.
	patch := map[string]any{
		"metadata": map[string]any{"annotations": resultAnnotations(a.ActionID, res)},
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{annotationRestartedAt: now},
		}}},
	}
	if _, err := e.Kube.PatchDeployment(ctx, ns, name, patch); err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	return res
}

func (e *Executor) scale(ctx context.Context, a protocol.Action, start time.Time) protocol.Result {
	ns, name := a.Params.Namespace, a.Params.Deployment
	if ns == "" || name == "" {
		return e.failed(start, "namespace and deployment parameters are required")
	}
	if a.Params.Replicas == nil || *a.Params.Replicas < 0 {
		return e.failed(start, "replica count missing or negative")
	}
	want := *a.Params.Replicas
	what := fmt.Sprintf("deployment %s/%s", ns, name)
	d, err := e.Kube.GetDeployment(ctx, ns, name)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(d.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	prev := 1 // Kubernetes default when replicas is not set
	if d.Spec.Replicas != nil {
		prev = int(*d.Spec.Replicas)
	}
	res := e.succeeded(start, map[string]any{"previous_replicas": prev, "replicas": want},
		"%s/%s scaled from %d to %d replicas", ns, name, prev, want)
	// A single patch on the deployment: replicas and annotations change together.
	patch := map[string]any{
		"metadata": map[string]any{"annotations": resultAnnotations(a.ActionID, res)},
		"spec":     map[string]any{"replicas": want},
	}
	if _, err := e.Kube.PatchDeployment(ctx, ns, name, patch); err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	return res
}

func (e *Executor) setSchedulable(ctx context.Context, a protocol.Action, start time.Time) protocol.Result {
	node := a.Params.Node
	if node == "" {
		return e.failed(start, "node parameter is required")
	}
	what := "node " + node
	n, err := e.Kube.GetNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(n.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	cordon := a.Type == protocol.ActionCordon
	msg := "node %s cordoned: no new pods will be scheduled on it"
	if !cordon {
		msg = "node %s uncordoned: pods can be scheduled on it again"
	}
	res := e.succeeded(start, map[string]any{"previously_unschedulable": n.Spec.Unschedulable}, msg, node)
	patch := map[string]any{
		"metadata": map[string]any{"annotations": resultAnnotations(a.ActionID, res)},
		"spec":     map[string]any{"unschedulable": cordon},
	}
	if _, err := e.Kube.PatchNode(ctx, node, patch); err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	return res
}

func podKey(p kube.Pod) string { return p.Metadata.Namespace + "/" + p.Metadata.Name }

func hasEmptyDir(p kube.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if len(v.EmptyDir) > 0 {
			return true
		}
	}
	return false
}

// drain follows the logic of "kubectl drain": it skips DaemonSet pods,
// mirror pods and terminated pods; it stops before cordoning the node if it
// finds pods that cannot be moved safely.
func (e *Executor) drain(ctx context.Context, a protocol.Action, start time.Time) protocol.Result {
	node := a.Params.Node
	if node == "" {
		return e.failed(start, "node parameter is required")
	}
	timeout := defaultDrainTimeout
	if a.Params.TimeoutSeconds > 0 {
		timeout = time.Duration(a.Params.TimeoutSeconds) * time.Second
	}
	what := "node " + node
	n, err := e.Kube.GetNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(n.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	pods, err := e.Kube.ListPodsOnNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, "pods on "+what))
	}

	var toEvict []kube.Pod
	var blockers []string
	skipped := map[string]int{}
	for _, p := range pods {
		ref := p.ControllerRef()
		switch {
		case p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed":
			skipped["terminated"]++
		case p.Metadata.Annotations[annotationMirrorPod] != "":
			skipped["static"]++
		case ref != nil && ref.Kind == "DaemonSet":
			skipped["daemonset"]++
		case ref == nil:
			blockers = append(blockers, podKey(p)+" is not managed by a controller and would be lost")
		case hasEmptyDir(p) && !a.Params.DeleteEmptyDirData:
			blockers = append(blockers, podKey(p)+" uses emptyDir volumes whose data would be lost")
		default:
			toEvict = append(toEvict, p)
		}
	}
	if len(blockers) > 0 {
		r := e.failed(start, "drain aborted before cordoning, the node was not changed: %s", strings.Join(blockers, "; "))
		r.Details = map[string]any{"blocking_pods": blockers}
		return r
	}

	if _, err := e.Kube.PatchNode(ctx, node, map[string]any{"spec": map[string]any{"unschedulable": true}}); err != nil {
		return e.failed(start, "%s", describe(err, what))
	}

	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Eviction in rounds: a pod blocked by a PodDisruptionBudget does not
	// prevent moving the others, and is retried in the next round.
	pending := toEvict
	for len(pending) > 0 {
		var blocked []kube.Pod
		for _, p := range pending {
			err := e.Kube.EvictPod(dctx, p.Metadata.Namespace, p.Metadata.Name)
			switch {
			case err == nil, kube.IsNotFound(err):
			case kube.IsTooManyRequests(err):
				blocked = append(blocked, p)
			case dctx.Err() != nil:
				return e.drainTimeout(start, node, timeout, append(blocked, p))
			default:
				return e.failed(start, "eviction of %s failed: %s; node %s stays cordoned", podKey(p), describe(err, "pod "+podKey(p)), node)
			}
		}
		pending = blocked
		if len(pending) > 0 && !sleep(dctx, e.PollInterval) {
			return e.drainTimeout(start, node, timeout, pending)
		}
	}

	// Wait until the pods are really gone from the node.
	remaining := toEvict
	for len(remaining) > 0 {
		var still []kube.Pod
		for _, p := range remaining {
			cur, err := e.Kube.GetPod(dctx, p.Metadata.Namespace, p.Metadata.Name)
			switch {
			case kube.IsNotFound(err):
			case err == nil && cur.Metadata.UID != p.Metadata.UID:
				// same name but a new pod (e.g. StatefulSet): the original is gone
			case err == nil:
				still = append(still, p)
			case dctx.Err() != nil:
				return e.drainTimeout(start, node, timeout, append(still, p))
			default:
				still = append(still, p)
			}
		}
		remaining = still
		if len(remaining) > 0 && !sleep(dctx, e.PollInterval) {
			return e.drainTimeout(start, node, timeout, remaining)
		}
	}

	evicted := make([]string, 0, len(toEvict))
	for _, p := range toEvict {
		evicted = append(evicted, podKey(p))
	}
	sort.Strings(evicted)
	res := e.succeeded(start, map[string]any{"evicted_pods": evicted, "skipped_pods": skipped},
		"node %s drained: %d pods evicted, %d skipped (DaemonSet, static or terminated)",
		node, len(evicted), skipped["daemonset"]+skipped["static"]+skipped["terminated"])
	patch := map[string]any{"metadata": map[string]any{"annotations": resultAnnotations(a.ActionID, res)}}
	if _, err := e.Kube.PatchNode(ctx, node, patch); err != nil {
		// The drain succeeded: if only the annotation is missing, a new delivery
		// will repeat a drain with no effect, so the result stays successful.
		res.Details["annotation_error"] = describe(err, what)
	}
	return res
}

func (e *Executor) drainTimeout(start time.Time, node string, timeout time.Duration, pods []kube.Pod) protocol.Result {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, podKey(p))
	}
	sort.Strings(names)
	r := e.failed(start, "drain of %s timed out after %s: pods not yet evicted: %s; the node stays cordoned",
		node, timeout, strings.Join(names, ", "))
	r.Details = map[string]any{"remaining_pods": names}
	return r
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
