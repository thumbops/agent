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
	"errors"
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

// AnnotationDrainInProgress marks a node whose drain started and has not
// finished: after a restart the agent finds it and resumes the drain.
const AnnotationDrainInProgress = "thumbops.mobiletechnologies.cloud/drain-in-progress"

// ErrActionGone is the cancellation cause used when the backend no longer
// tracks the action (409/410 on progress): the drain stops, removes its
// in-progress annotation and no result is sent. Any other cancellation (the
// agent shutting down) keeps the annotation, so the drain is resumed.
var ErrActionGone = errors.New("the backend no longer tracks the action")

// ProgressFunc receives the progress of a long action. It may be nil.
type ProgressFunc func(protocol.Progress)

// drainState is the value of AnnotationDrainInProgress.
type drainState struct {
	ActionID           string    `json:"action_id"`
	StartedAt          time.Time `json:"started_at"`
	TimeoutSeconds     int       `json:"timeout_seconds"`
	DeleteEmptyDirData bool      `json:"delete_emptydir_data"`
}

func readDrainState(ann map[string]string) (drainState, bool) {
	raw, ok := ann[AnnotationDrainInProgress]
	if !ok {
		return drainState{}, false
	}
	var st drainState
	if err := json.Unmarshal([]byte(raw), &st); err != nil || st.ActionID == "" || st.StartedAt.IsZero() {
		return drainState{}, false
	}
	return st, true
}

const maxBlockedInProgress = 20

type Executor struct {
	Kube         *kube.Client
	Now          func() time.Time
	PollInterval time.Duration // wait between attempts during a drain

	// SelfNamespace and SelfName identify the agent's own pod, which a drain
	// never evicts. Both empty = unknown.
	SelfNamespace, SelfName string
}

func New(k *kube.Client) *Executor {
	return &Executor{Kube: k, Now: time.Now, PollInterval: 2 * time.Second}
}

// Execute runs the action and always returns a result, never an error:
// a failure is also a result to report to the backend.
func (e *Executor) Execute(ctx context.Context, a protocol.Action, progress ProgressFunc) protocol.Result {
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
		res = e.drain(ctx, a, start, progress)
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
	ann := resultAnnotations(a.ActionID, res)
	if !cordon {
		// An explicit uncordon ends a drain in progress: it must not be resumed.
		ann[AnnotationDrainInProgress] = nil
	}
	patch := map[string]any{
		"metadata": map[string]any{"annotations": ann},
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

// InProgressDrains returns the drains started and not finished (nodes with
// AnnotationDrainInProgress), rebuilt as actions to resume. Unreadable
// annotations are removed and their nodes returned in dropped.
func (e *Executor) InProgressDrains(ctx context.Context) (actions []protocol.Action, dropped []string, err error) {
	nodes, err := e.Kube.ListNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range nodes {
		name := n.Metadata.Name
		if _, ok := n.Metadata.Annotations[AnnotationDrainInProgress]; !ok {
			continue
		}
		st, ok := readDrainState(n.Metadata.Annotations)
		if !ok {
			if _, perr := e.Kube.PatchNode(ctx, name, map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: nil}}}); perr != nil {
				dropped = append(dropped, fmt.Sprintf("%s (annotation not removed: %v)", name, perr))
			} else {
				dropped = append(dropped, name)
			}
			continue
		}
		actions = append(actions, protocol.Action{ActionID: st.ActionID, Type: protocol.ActionDrain, Params: protocol.Params{
			Node: name, TimeoutSeconds: st.TimeoutSeconds, DeleteEmptyDirData: st.DeleteEmptyDirData}})
	}
	return actions, dropped, nil
}

// drain follows the logic of "kubectl drain": it skips DaemonSet pods,
// mirror pods, terminated pods and the agent's own pod; it stops before
// cordoning the node if it finds pods that cannot be moved safely.
func (e *Executor) drain(ctx context.Context, a protocol.Action, start time.Time, progress ProgressFunc) protocol.Result {
	node := a.Params.Node
	if node == "" {
		return e.failed(start, "node parameter is required")
	}
	total := defaultDrainTimeout
	if a.Params.TimeoutSeconds > 0 {
		total = time.Duration(a.Params.TimeoutSeconds) * time.Second
	}
	what := "node " + node
	n, err := e.Kube.GetNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(n.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	// This action's drain already started before an agent restart: resume
	// it with the time left.
	left := total
	st, resumed := readDrainState(n.Metadata.Annotations)
	resumed = resumed && st.ActionID == a.ActionID
	if resumed {
		start = st.StartedAt.UTC()
		left = total - e.Now().Sub(st.StartedAt)
	}
	pods, err := e.Kube.ListPodsOnNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, "pods on "+what))
	}

	var toEvict []kube.Pod
	var blockers []string
	var self string
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
		case e.isSelf(p):
			skipped["agent"]++
			self = podKey(p)
		case ref == nil:
			blockers = append(blockers, podKey(p)+" is not managed by a controller and would be lost")
		case hasEmptyDir(p) && !a.Params.DeleteEmptyDirData:
			blockers = append(blockers, podKey(p)+" uses emptyDir volumes whose data would be lost")
		default:
			toEvict = append(toEvict, p)
		}
	}
	if len(blockers) > 0 && !resumed {
		r := e.failed(start, "drain aborted before cordoning, the node was not changed: %s", strings.Join(blockers, "; "))
		r.Details = map[string]any{"blocking_pods": blockers}
		return r
	}
	if len(blockers) > 0 {
		r := e.failed(start, "drain of %s aborted on resume: %s; the node stays cordoned", node, strings.Join(blockers, "; "))
		r.Details = map[string]any{"blocking_pods": blockers}
		return e.finishDrain(ctx, a, node, r)
	}

	if resumed {
		// Re-assert the cordon: the node may have been uncordoned by hand
		// while the agent was down.
		if _, err := e.Kube.PatchNode(ctx, node, map[string]any{"spec": map[string]any{"unschedulable": true}}); err != nil {
			return e.failed(start, "%s", describe(err, what))
		}
	} else {
		state, _ := json.Marshal(drainState{ActionID: a.ActionID, StartedAt: start, TimeoutSeconds: int(total / time.Second),
			DeleteEmptyDirData: a.Params.DeleteEmptyDirData})
		patch := map[string]any{
			"spec":     map[string]any{"unschedulable": true},
			"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: string(state)}},
		}
		if _, err := e.Kube.PatchNode(ctx, node, patch); err != nil {
			return e.failed(start, "%s", describe(err, what))
		}
	}
	report := func(evicted, remaining int, blocked []kube.Pod, waiting bool) {
		if progress == nil {
			return
		}
		list := make([]map[string]string, 0, min(len(blocked), maxBlockedInProgress))
		for _, p := range blocked[:min(len(blocked), maxBlockedInProgress)] {
			list = append(list, map[string]string{"pod": podKey(p), "reason": "PodDisruptionBudget"})
		}
		msg := fmt.Sprintf("draining %s: %d pods evicted, %d remaining", node, evicted, remaining)
		if waiting {
			msg = fmt.Sprintf("draining %s: %d pods evicted, %d still terminating", node, evicted, remaining)
		}
		if len(blocked) > 0 {
			msg += fmt.Sprintf(", %d blocked by a PodDisruptionBudget", len(blocked))
		}
		progress(protocol.Progress{UpdatedAt: e.Now().UTC(), Message: msg,
			Details: map[string]any{"evicted": evicted, "remaining": remaining, "blocked": list}})
	}
	report(0, len(toEvict), nil, false)
	if left <= 0 {
		return e.finishDrain(ctx, a, node, e.drainTimeout(start, node, total, toEvict))
	}

	dctx, cancel := context.WithTimeout(ctx, left)
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
				return e.finishDrain(ctx, a, node, e.drainTimeout(start, node, total, append(blocked, p)))
			default:
				return e.finishDrain(ctx, a, node, e.failed(start, "eviction of %s failed: %s; node %s stays cordoned", podKey(p), describe(err, "pod "+podKey(p)), node))
			}
		}
		pending = blocked
		report(len(toEvict)-len(pending), len(pending), pending, false)
		if len(pending) > 0 && !sleep(dctx, e.PollInterval) {
			return e.finishDrain(ctx, a, node, e.drainTimeout(start, node, total, pending))
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
				return e.finishDrain(ctx, a, node, e.drainTimeout(start, node, total, append(still, p)))
			default:
				still = append(still, p)
			}
		}
		remaining = still
		report(len(toEvict), len(remaining), nil, true)
		if len(remaining) > 0 && !sleep(dctx, e.PollInterval) {
			return e.finishDrain(ctx, a, node, e.drainTimeout(start, node, total, remaining))
		}
	}

	evicted := make([]string, 0, len(toEvict))
	for _, p := range toEvict {
		evicted = append(evicted, podKey(p))
	}
	sort.Strings(evicted)
	details := map[string]any{"evicted_pods": evicted, "skipped_pods": skipped}
	msg := fmt.Sprintf("node %s drained: %d pods evicted, %d skipped (DaemonSet, static or terminated)",
		node, len(evicted), skipped["daemonset"]+skipped["static"]+skipped["terminated"])
	if self != "" {
		details["agent_pod_left"] = self
		msg += fmt.Sprintf("; the agent's own pod %s stays until the agent restarts (the node is cordoned)", self)
	}
	res := e.succeeded(start, details, "%s", msg)
	return e.finishDrain(ctx, a, node, res)
}

// finishDrain is the single exit after the cordon. If the backend no longer
// tracks the action (ErrActionGone) it removes the in-progress annotation
// and nothing else; if the agent is shutting down it keeps it, so the drain
// is resumed; otherwise it removes it and writes the result annotations in
// one patch.
func (e *Executor) finishDrain(ctx context.Context, a protocol.Action, node string, res protocol.Result) protocol.Result {
	if ctx.Err() != nil {
		cause := context.Cause(ctx)
		res = e.failed(res.StartedAt, "drain of %s stopped: %v; the node stays cordoned", node, cause)
		if errors.Is(cause, ErrActionGone) {
			_, _ = e.Kube.PatchNode(context.WithoutCancel(ctx), node, map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: nil}}})
		}
		return res
	}
	ann := resultAnnotations(a.ActionID, res)
	ann[AnnotationDrainInProgress] = nil
	if _, err := e.Kube.PatchNode(ctx, node, map[string]any{"metadata": map[string]any{"annotations": ann}}); err != nil {
		// The drain itself is done: if only the annotation is missing, a new
		// delivery repeats a drain with no effect, so the result stands.
		if res.Details == nil {
			res.Details = map[string]any{}
		}
		res.Details["annotation_error"] = describe(err, "node "+node)
	}
	return res
}

func (e *Executor) isSelf(p kube.Pod) bool {
	return e.SelfName != "" && e.SelfNamespace != "" &&
		p.Metadata.Name == e.SelfName && p.Metadata.Namespace == e.SelfNamespace
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
