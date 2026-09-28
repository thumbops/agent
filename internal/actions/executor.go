// Package actions esegue le azioni del protocollo sul cluster.
//
// Ogni azione è idempotente: l'agente annota la risorsa modificata con
// l'action_id e l'esito, nella stessa patch che applica la modifica. Se
// l'agente si riavvia tra l'esecuzione e l'invio dell'esito, alla nuova
// consegna trova l'annotazione e reinvia l'esito invece di ripetere l'azione.
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
	PollInterval time.Duration // attesa tra i tentativi durante il drain
}

func New(k *kube.Client) *Executor {
	return &Executor{Kube: k, Now: time.Now, PollInterval: 2 * time.Second}
}

// Execute esegue l'azione e restituisce sempre un esito, mai un errore:
// anche un fallimento è un esito da comunicare al backend.
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
		res = protocol.Result{Status: protocol.StatusRejected, Message: fmt.Sprintf("tipo di azione sconosciuto %q", a.Type)}
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

// describe traduce un errore dell'API in un messaggio per chi è di turno.
func describe(err error, what string) string {
	switch {
	case kube.IsNotFound(err):
		return what + " non trovato"
	case kube.IsForbidden(err):
		return "permesso negato su " + what + ": verificare il ClusterRole dell'agente"
	default:
		return "errore dell'API Kubernetes su " + what + ": " + err.Error()
	}
}

// previousResult riconosce un'azione già eseguita.
func previousResult(ann map[string]string, actionID string) (protocol.Result, bool) {
	if actionID == "" || ann[AnnotationLastActionID] != actionID {
		return protocol.Result{}, false
	}
	var r protocol.Result
	if err := json.Unmarshal([]byte(ann[AnnotationLastResult]), &r); err != nil || r.Status == "" {
		r = protocol.Result{Status: protocol.StatusSucceeded, Message: "azione già eseguita in precedenza"}
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
		return e.failed(start, "parametri namespace e deployment obbligatori")
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
	res := e.succeeded(start, map[string]any{"restarted_at": now}, "rollout di %s/%s avviato", ns, name)
	// Stesso meccanismo di "kubectl rollout restart": cambiare un'annotazione
	// del template fa partire un nuovo rollout.
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
		return e.failed(start, "parametri namespace e deployment obbligatori")
	}
	if a.Params.Replicas == nil || *a.Params.Replicas < 0 {
		return e.failed(start, "numero di repliche mancante o negativo")
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
	prev := 1 // default di Kubernetes quando replicas non è impostato
	if d.Spec.Replicas != nil {
		prev = int(*d.Spec.Replicas)
	}
	res := e.succeeded(start, map[string]any{"previous_replicas": prev, "replicas": want},
		"%s/%s scalato da %d a %d repliche", ns, name, prev, want)
	// Una sola patch sul deployment: repliche e annotazioni cambiano insieme.
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
		return e.failed(start, "parametro node obbligatorio")
	}
	what := "nodo " + node
	n, err := e.Kube.GetNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(n.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	cordon := a.Type == protocol.ActionCordon
	msg := "nodo %s messo in cordon: nessun nuovo pod verrà pianificato"
	if !cordon {
		msg = "nodo %s riattivato (uncordon)"
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

// drain segue la logica di "kubectl drain": ignora i pod dei DaemonSet, i
// mirror pod e quelli terminati; si ferma prima di mettere il nodo in cordon
// se trova pod che non si possono spostare in sicurezza.
func (e *Executor) drain(ctx context.Context, a protocol.Action, start time.Time) protocol.Result {
	node := a.Params.Node
	if node == "" {
		return e.failed(start, "parametro node obbligatorio")
	}
	timeout := defaultDrainTimeout
	if a.Params.TimeoutSeconds > 0 {
		timeout = time.Duration(a.Params.TimeoutSeconds) * time.Second
	}
	what := "nodo " + node
	n, err := e.Kube.GetNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, what))
	}
	if r, ok := previousResult(n.Metadata.Annotations, a.ActionID); ok {
		return r
	}
	pods, err := e.Kube.ListPodsOnNode(ctx, node)
	if err != nil {
		return e.failed(start, "%s", describe(err, "pod del "+what))
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
			blockers = append(blockers, podKey(p)+" non è gestito da un controller e andrebbe perso")
		case hasEmptyDir(p) && !a.Params.DeleteEmptyDirData:
			blockers = append(blockers, podKey(p)+" usa volumi emptyDir, i cui dati andrebbero persi")
		default:
			toEvict = append(toEvict, p)
		}
	}
	if len(blockers) > 0 {
		r := e.failed(start, "drain annullato prima del cordon, il nodo non è stato modificato: %s", strings.Join(blockers, "; "))
		r.Details = map[string]any{"blocking_pods": blockers}
		return r
	}

	if _, err := e.Kube.PatchNode(ctx, node, map[string]any{"spec": map[string]any{"unschedulable": true}}); err != nil {
		return e.failed(start, "%s", describe(err, what))
	}

	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Eviction a turni: un pod bloccato da un PodDisruptionBudget non
	// impedisce di spostare gli altri, e viene riprovato al turno successivo.
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
				return e.failed(start, "eviction di %s fallita: %s; il nodo %s resta in cordon", podKey(p), describe(err, "pod "+podKey(p)), node)
			}
		}
		pending = blocked
		if len(pending) > 0 && !sleep(dctx, e.PollInterval) {
			return e.drainTimeout(start, node, timeout, pending)
		}
	}

	// Attende che i pod spariscano davvero dal nodo.
	remaining := toEvict
	for len(remaining) > 0 {
		var still []kube.Pod
		for _, p := range remaining {
			cur, err := e.Kube.GetPod(dctx, p.Metadata.Namespace, p.Metadata.Name)
			switch {
			case kube.IsNotFound(err):
			case err == nil && cur.Metadata.UID != p.Metadata.UID:
				// stesso nome ma pod nuovo (es. StatefulSet): l'originale è andato
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
		"nodo %s svuotato: pod spostati %d, ignorati %d (DaemonSet, statici o terminati)",
		node, len(evicted), skipped["daemonset"]+skipped["static"]+skipped["terminated"])
	patch := map[string]any{"metadata": map[string]any{"annotations": resultAnnotations(a.ActionID, res)}}
	if _, err := e.Kube.PatchNode(ctx, node, patch); err != nil {
		// Il drain è riuscito: se manca solo l'annotazione, una nuova consegna
		// rifarà un drain senza effetti, quindi l'esito resta positivo.
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
	r := e.failed(start, "timeout di %s scaduto durante il drain di %s: pod non ancora spostati: %s; il nodo resta in cordon",
		timeout, node, strings.Join(names, ", "))
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
