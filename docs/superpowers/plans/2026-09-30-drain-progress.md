# Drain Progress, Resume and Own Pod Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Long drains report progress that renews the action's lease, a drain interrupted by an agent restart is resumed, and a drain never evicts the agent's own pod.

**Architecture:** The protocol gains `POST /v1/agent/actions/{id}/progress` (spec repository first). The executor records an in-progress annotation on the node together with the cordon, calls a progress callback during the drain, detects a drain it already started (resume) and skips its own pod. The agent throttles progress through a reporter that stops the action on `409`/`410`, and resumes annotated drains at startup.

**Tech Stack:** Go 1.26, the repository's `kube` REST client and `kubefake`, the mock backend, bash + kind for end-to-end tests; Python validator in `thumbops/spec`.

**Spec:** `docs/superpowers/specs/2026-09-30-drain-progress-design.md`

## Global Constraints

- Everything written into both repositories is in English: code, comments, logs, errors, docs, commit messages.
- Protocol stays v1: progress is an optional, compatible addition; a `404` on progress means "not supported" and the action goes on.
- Progress rules: first report right after the cordon; a change at most once every 5 s; an unchanged state at least every 30 s; `blocked` has at most 20 entries; progress is never retried.
- `409`/`410` on progress: stop the action (no further evictions), leave the node cordoned, remove the in-progress annotation, send no result.
- Lease: a claimed action expires 5 minutes after the claim or the last progress, whichever is later, without a result.
- Annotation `thumbops.mobiletechnologies.cloud/drain-in-progress` = JSON `{"action_id","started_at","timeout_seconds","delete_emptydir_data"}`, written in the same merge patch as `spec.unschedulable: true`, removed in the same patch that writes `last-action-id`/`last-action-result`. An agent shutdown (not a `409`/`410`) keeps it.
- The drain never evicts the agent's own pod; it reports it in `details.agent_pod_left`.
- Do not break the invariants in `CLAUDE.md`; `go test -race ./...`, `go vet ./...`, `gofmt -l .` (empty) and `test/chart/check.sh` must pass after every task.
- kind/kubectl/helm for tests only with `export KUBECONFIG=<dedicated file>` (never `~/.kube/config`, never `kubectl config use-context`), only on the kind cluster `thumbops-e2e`.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

---

### Task 1: Protocol spec (repository thumbops/spec)

**Files (in a worktree of `thumbops/spec`, not in the agent repository):**
- Modify: `protocol/protocol.md`
- Modify: `protocol/openapi.yaml`

**Interfaces:**
- Produces: the contract the other tasks implement (endpoint, body, responses, lease, resume, own pod).

- [ ] **Step 1: Worktree**

```bash
git -C /Volumes/DataDisk/Sviluppo/personal/thumbsOps/spec fetch origin
git -C /Volumes/DataDisk/Sviluppo/personal/thumbsOps/spec worktree add -b claude/drain-progress <SCRATCH>/spec-drain-progress origin/main
```

`<SCRATCH>` is the scratch directory given in your dispatch. Work only there; never change the branch of the main `spec` checkout.

- [ ] **Step 2: protocol.md**

1. In "Receiving actions", replace the `drain` row of the parameters table with:

```markdown
| `drain` | `node`, `timeout_seconds` (default 600), `delete_emptydir_data` (default false). The agent never evicts its own pod: it is skipped like DaemonSet pods and reported in the result (`details.agent_pod_left`); the node is cordoned, so the agent moves at its next restart. |
```

2. In "Execution and results", right after the paragraph that starts with "`status` is `succeeded`", add:

````markdown
**Progress.** A long action (today only `drain`) reports its progress while it runs with `POST /v1/agent/actions/{id}/progress`:

```json
{
  "updated_at": "2026-09-30T10:03:12Z",
  "message": "draining worker-3: 14 pods evicted, 2 remaining",
  "details": {
    "evicted": 14,
    "remaining": 2,
    "blocked": [{ "pod": "payments/api-7f9c", "reason": "PodDisruptionBudget" }]
  }
}
```

`details` depends on the action type; for `drain`, `blocked` lists at most 20 pods the agent cannot evict yet. The agent sends the first progress right after the cordon, then whenever the counts change (at most once every 5 seconds) and at least every 30 seconds. Progress is never retried: the next one is newer.

The backend answers `200` (empty body), stores the progress for the dashboard and renews the action's lease (see the states below). `409` or `410` mean the backend no longer tracks the action (expired or cancelled): the agent stops it, with no further evictions, leaves the node cordoned and sends no result. `404` means the backend does not support progress: the agent stops sending it for that action and goes on.

**Resume.** When the drain starts, the agent writes the annotation `thumbops.mobiletechnologies.cloud/drain-in-progress` on the node, in the same patch as the cordon, with the `action_id`, the start time and the parameters. If the agent restarts, it finds the annotation and resumes the drain with the time left, without a new claim: it sends progress and the result for the same `action_id`, which the backend accepts while the action is `claimed`. The annotation is removed in the patch that writes the result, or when the backend answers `409`/`410` to the progress.
````

3. In the states table, replace the `expired` row with:

```markdown
| `expired` | Backend | Not claimed before the deadline, or claimed and then 5 minutes without a result or a progress (each progress renews the lease) |
```

4. In "Idempotency", replace the sentence that starts with "The drain is the exception" with:

```markdown
The drain is the exception: it is made of several steps. It marks the node with the `drain-in-progress` annotation in the cordon patch and writes the result annotations, removing the in-progress one, only at the end; repeating it on an already drained node has no effect.
```

5. In "Open questions", delete the line about the drain and `progress`.

- [ ] **Step 3: openapi.yaml**

1. After the `/v1/agent/actions/{action_id}/result:` path block (before the next top-level path or `components:`), add:

```yaml
  /v1/agent/actions/{action_id}/progress:
    post:
      tags: [actions]
      operationId: sendProgress
      summary: Report the progress of a claimed long action
      description: |
        Sent for long actions only (today `drain`): right after the cordon,
        then when the counts change (at most once every 5 s) and at least
        every 30 s. Never retried. Each accepted progress renews the lease of
        the claimed action (5 minutes). Also sent after an agent restart for
        a drain it resumes, without a new claim.
      parameters:
        - $ref: '#/components/parameters/UserAgent'
        - $ref: '#/components/parameters/ActionID'
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/Progress' }
            example:
              updated_at: '2026-09-30T10:03:12Z'
              message: 'draining worker-3: 14 pods evicted, 2 remaining'
              details:
                evicted: 14
                remaining: 2
                blocked:
                  - { pod: payments/api-7f9c, reason: PodDisruptionBudget }
      responses:
        '200':
          description: Progress recorded and lease renewed. Empty body.
        '400': { $ref: '#/components/responses/BadRequest' }
        '401': { $ref: '#/components/responses/Unauthorized' }
        '404':
          description: Unknown action, or progress not supported. The agent stops sending progress for this action and goes on.
          content:
            text/plain:
              schema: { type: string }
        '409':
          description: The action is not claimed by this agent. The agent stops the action and sends no result.
          content:
            text/plain:
              schema: { type: string }
        '410':
          description: The action expired or was cancelled. The agent stops the action, leaves the node cordoned and sends no result.
          content:
            text/plain:
              schema: { type: string }
        '429': { $ref: '#/components/responses/TooManyRequests' }
        5XX: { $ref: '#/components/responses/ServerError' }
```

Check that the parameters and responses referenced (`UserAgent`, `ActionID`, `BadRequest`, `Unauthorized`, `TooManyRequests`, `ServerError`) exist in `components` under exactly these names (they are used by the result path); if a name differs, use the one the result path uses.

2. In `components.schemas`, after `Result`, add:

```yaml
    Progress:
      type: object
      required: [updated_at, message]
      properties:
        updated_at: { $ref: '#/components/schemas/Timestamp' }
        message:
          type: string
          description: Human-readable progress, shown in the app.
        details:
          type: object
          description: |
            Action-specific data. For `drain`: `evicted` and `remaining`
            (integers) and `blocked`, at most 20 objects with `pod`
            (`namespace/name`) and `reason`.
```

3. In the `Result` schema `details` description, add `agent_pod_left` to the examples.

- [ ] **Step 4: Validate and commit**

```bash
cd <SCRATCH>/spec-drain-progress/protocol
python3 -m venv <SCRATCH>/spec-venv && <SCRATCH>/spec-venv/bin/pip install -q -r requirements.txt
<SCRATCH>/spec-venv/bin/python validate.py
```

Expected: exit 0 with no errors.

```bash
git -C <SCRATCH>/spec-drain-progress add protocol
git -C <SCRATCH>/spec-drain-progress commit -m "Add progress for long actions, drain resume and the agent's own pod

Closes the open question on long drains: a claimed action is renewed by
progress, a drain interrupted by an agent restart is resumed, and the agent
never evicts its own pod.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Do not push: the controller pushes and opens the PR.

---

### Task 2: Progress in the protocol types, backend client, metrics and mock backend

**Files:**
- Modify: `internal/protocol/types.go`, `internal/backend/client.go`, `internal/metrics/metrics.go`, `internal/mockbackend/server.go`
- Test: `internal/backend/backend_test.go`, `internal/mockbackend/server_test.go` (new)

**Interfaces:**
- Produces:
  - `protocol.Progress{UpdatedAt time.Time "updated_at"; Message string "message"; Details map[string]any "details,omitempty"}`
  - `func (c *backend.Client) SendProgress(ctx context.Context, actionID string, p protocol.Progress) error` (timeout 10 s, metrics operation `progress`)
  - `metrics.OpProgress = "progress"`
  - mock backend: route `POST /v1/agent/actions/{id}/progress`; field `ClaimLease time.Duration` (default 5 min); methods `Progress(id string) (protocol.Progress, int)`, `Cancel(id string)`, `EnqueueClaimed(a protocol.Action)`; route `POST /debug/actions/{id}/cancel`; `/debug/actions` rows gain `progress` and `progress_count`.

- [ ] **Step 1: Write the failing tests**

`internal/mockbackend/server_test.go`:

```go
package mockbackend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/protocol"
)

func post(t *testing.T, h http.Handler, path string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return rec.Code
}

func TestProgressRenewsTheLease(t *testing.T) {
	s := New()
	s.ClaimLease = 100 * time.Millisecond
	h := s.Handler()
	s.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	if code := post(t, h, "/v1/agent/actions/d1/claim", nil); code != http.StatusOK {
		t.Fatalf("claim: %d", code)
	}
	p := protocol.Progress{UpdatedAt: time.Now(), Message: "draining"}
	for range 3 { // three renewals, each before the lease ends
		time.Sleep(60 * time.Millisecond)
		if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusOK {
			t.Fatalf("progress within the lease: %d", code)
		}
	}
	if last, n := s.Progress("d1"); n != 3 || last.Message != "draining" {
		t.Fatalf("progress stored: %d %+v", n, last)
	}
	time.Sleep(150 * time.Millisecond) // lease over
	if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusGone {
		t.Fatalf("progress after the lease: %d, want 410", code)
	}
	if code := post(t, h, "/v1/agent/actions/d1/result", protocol.Result{Status: protocol.StatusSucceeded}); code != http.StatusGone {
		t.Fatalf("result after the lease: %d, want 410", code)
	}
}

func TestProgressOnCancelledAndUnknownActions(t *testing.T) {
	s := New()
	h := s.Handler()
	s.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	p := protocol.Progress{UpdatedAt: time.Now(), Message: "draining"}
	if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusOK {
		t.Fatalf("progress on an action claimed before a restart: %d", code)
	}
	if code := post(t, h, "/debug/actions/d1/cancel", nil); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
	if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusGone {
		t.Fatalf("progress on a cancelled action: %d, want 410", code)
	}
	if code := post(t, h, "/v1/agent/actions/nope/progress", p); code != http.StatusNotFound {
		t.Fatalf("progress on an unknown action: %d, want 404", code)
	}
}
```

Append to `internal/backend/backend_test.go` (uses the existing helpers `startTLS`, `register`, `newClient`, `scrape`):

```go
func TestSendProgress(t *testing.T) {
	mb, srv, roots := startTLS(t)
	m := metrics.New("test")
	holder := &identity.Holder{}
	c := backend.New(backend.Options{BaseURL: srv.URL, TLS: holder.ClientTLS(roots), Metrics: m})
	e := &enroll.Enroller{Backend: c, Store: identity.FileStore{Dir: t.TempDir()}, Holder: holder}
	if _, err := e.Start(context.Background(), "bootstrap-test-token", func(context.Context) (protocol.RegisterRequest, error) { return info, nil }); err != nil {
		t.Fatal(err)
	}
	mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	p := protocol.Progress{UpdatedAt: time.Now().UTC(), Message: "draining worker-1: 1 pods evicted, 1 remaining",
		Details: map[string]any{"evicted": 1, "remaining": 1}}
	if err := c.SendProgress(context.Background(), "d1", p); err != nil {
		t.Fatal(err)
	}
	if last, n := mb.Progress("d1"); n != 1 || last.Message != p.Message {
		t.Fatalf("progress received: %d %+v", n, last)
	}
	if err := c.SendProgress(context.Background(), "unknown", p); backend.Code(err) != http.StatusNotFound {
		t.Fatalf("unknown action: %v", err)
	}
	body := scrape(t, m)
	for _, line := range []string{
		`thumbops_agent_backend_requests_total{code="200",operation="progress"} 1`,
		`thumbops_agent_backend_requests_total{code="404",operation="progress"} 1`,
	} {
		if !strings.Contains(body, line) {
			t.Errorf("missing %s", line)
		}
	}
}
```

Run: `go test ./internal/mockbackend/ ./internal/backend/`
Expected: FAIL (build errors: `ClaimLease`, `Progress`, `EnqueueClaimed`, `SendProgress` undefined).

- [ ] **Step 2: Protocol type and metrics constant**

In `internal/protocol/types.go`, after `Result`:

```go
// Progress is the intermediate state of a long action (today only drain),
// sent with POST /v1/agent/actions/{id}/progress. For drain, Details has
// "evicted", "remaining" and "blocked" (at most 20 {"pod", "reason"}).
type Progress struct {
	UpdatedAt time.Time      `json:"updated_at"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
}
```

In `internal/metrics/metrics.go`, add `OpProgress = "progress"` to the operation constants (after `OpStatus`) and mention `progress` in the `backend_requests_total` help if it lists operations.

- [ ] **Step 3: Backend client**

In `internal/backend/client.go`, after `SendResult`:

```go
// progressTimeout is short: progress is never retried and must not hold up
// the action it reports on.
const progressTimeout = 10 * time.Second

// SendProgress reports the progress of a claimed long action. It is never
// retried: the next progress is newer.
func (c *Client) SendProgress(ctx context.Context, actionID string, p protocol.Progress) error {
	_, err := c.do(ctx, metrics.OpProgress, http.MethodPost, "/v1/agent/actions/"+url.PathEscape(actionID)+"/progress", "", progressTimeout, p, nil)
	return err
}
```

- [ ] **Step 4: Mock backend**

In `internal/mockbackend/server.go`:

1. `entry` gains:

```go
	leaseUntil    time.Time // a claimed action expires after this without a result
	progress      protocol.Progress
	progressCount int
```

2. `Server` gains the exported field `ClaimLease time.Duration // lease of a claimed action, renewed by progress`, set to `5 * time.Minute` in `New()`.

3. Add:

```go
// expireLeases marks as expired the claimed actions whose lease is over.
// Call it with s.mu held.
func (s *Server) expireLeases() {
	now := time.Now()
	for _, e := range s.entries {
		if e.state == stateClaimed && !e.leaseUntil.IsZero() && now.After(e.leaseUntil) {
			e.state = stateExpired
		}
	}
}

// EnqueueClaimed adds an action already claimed, as if the agent had claimed
// it before a restart.
func (s *Server) EnqueueClaimed(a protocol.Action) {
	s.Enqueue(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byID[a.ActionID]
	e.state = stateClaimed
	e.leaseUntil = time.Now().Add(s.ClaimLease)
}

// Cancel cancels an action that is approved or claimed (the next progress
// gets 410).
func (s *Server) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok && (e.state == stateApproved || e.state == stateClaimed) {
		e.state = stateCancelled
	}
}

// Progress returns the last progress of an action and how many were received.
func (s *Server) Progress(id string) (protocol.Progress, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return protocol.Progress{}, 0
	}
	return e.progress, e.progressCount
}

func (s *Server) progressHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p protocol.Progress
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLeases()
	e, ok := s.byID[id]
	switch {
	case !ok:
		http.Error(w, "unknown action", http.StatusNotFound)
	case e.state == stateClaimed:
		e.progress = p
		e.progressCount++
		e.leaseUntil = time.Now().Add(s.ClaimLease)
		w.WriteHeader(http.StatusOK)
	case e.state == stateExpired || e.state == stateCancelled:
		http.Error(w, "action expired or cancelled", http.StatusGone)
	default:
		http.Error(w, "action not claimed", http.StatusConflict)
	}
}

func (s *Server) debugCancel(w http.ResponseWriter, r *http.Request) {
	s.Cancel(r.PathValue("id"))
	w.WriteHeader(http.StatusOK)
}
```

4. In `claim`, in the `case e.state == stateApproved:` branch, also set `e.leaseUntil = time.Now().Add(s.ClaimLease)`. At the top of `claim` (after `s.mu.Lock()`), call `s.expireLeases()`.

5. In `result`, after `s.mu.Lock()` (and the `ResultFailures` block), call `s.expireLeases()`, and before the existing `!ok || e.state != stateClaimed` check add:

```go
	if ok && (e.state == stateExpired || e.state == stateCancelled) {
		http.Error(w, "action expired or cancelled", http.StatusGone)
		return
	}
```

6. In `Handler`, register:

```go
	mux.HandleFunc("POST /v1/agent/actions/{id}/progress", s.progressHandler)
	mux.HandleFunc("POST /debug/actions/{id}/cancel", s.debugCancel)
```

7. In `debugList`, call `s.expireLeases()` after locking, add to `row`:

```go
		Progress      *protocol.Progress `json:"progress,omitempty"`
		ProgressCount int                `json:"progress_count,omitempty"`
```

and fill them when `e.progressCount > 0` (`p := e.progress; rw.Progress = &p; rw.ProgressCount = e.progressCount`).

8. In the package comment of `cmd/mock-backend/main.go`, add the cancel route to the list of debug commands:

```
//	curl -X POST localhost:8080/debug/actions/<id>/cancel
```

- [ ] **Step 5: Run the tests and commit**

Run: `go test -race ./... && go vet ./... && gofmt -l .`
Expected: PASS, nothing from gofmt.

```bash
git add internal/protocol internal/backend internal/metrics internal/mockbackend cmd/mock-backend
git commit -m "Add progress to the protocol client and the mock backend

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Drain progress, in-progress annotation, resume detection and own pod in the executor

**Files:**
- Modify: `internal/actions/executor.go`
- Test: `internal/actions/executor_test.go`

**Interfaces:**
- Consumes: `protocol.Progress` (Task 2).
- Produces:
  - `const AnnotationDrainInProgress = "thumbops.mobiletechnologies.cloud/drain-in-progress"`
  - `var ErrActionGone error` (cancellation cause: the backend no longer tracks the action)
  - `type ProgressFunc func(protocol.Progress)` (nil = no progress)
  - `func (e *Executor) Execute(ctx context.Context, a protocol.Action, progress ProgressFunc) protocol.Result` (**signature change**: every caller passes the new argument, `nil` when there is no reporter)
  - `Executor.SelfNamespace`, `Executor.SelfName string` (the agent's own pod; both empty = unknown)
  - `func (e *Executor) InProgressDrains(ctx context.Context) (actions []protocol.Action, dropped []string, err error)`

- [ ] **Step 1: Update the existing callers of Execute**

Run: `grep -rn "\.Execute(" --include=*.go .`
Change every call to pass a third argument `nil` (tests in `internal/actions`, and `internal/agent/agent.go` `handle` for now; Task 4 passes a real reporter).

- [ ] **Step 2: Write the failing tests**

Append to `internal/actions/executor_test.go`:

```go
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
	r := e.Execute(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}}, log.add)
	expectStatus(t, r, protocol.StatusSucceeded)

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
```

These tests need a kubefake helper to set node state directly. Add to `internal/kubefake/server.go`:

```go
// PatchNodeForTest applies a JSON merge patch to a node, as a test setup
// step (no permission check, not recorded in Patches).
func (s *Server) PatchNodeForTest(name string, patch map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[name]
	cur := map[string]any{}
	b, _ := json.Marshal(n)
	_ = json.Unmarshal(b, &cur)
	merged := mergePatch(cur, patch)
	b, _ = json.Marshal(merged)
	var out kube.Node
	_ = json.Unmarshal(b, &out)
	s.nodes[name] = &out
}
```

(Check that `encoding/json` is imported in `kubefake/server.go`; add it if not.)

Run: `go test ./internal/actions/`
Expected: FAIL (build errors: `AnnotationDrainInProgress`, `ErrActionGone`, `InProgressDrains`, `SelfNamespace` undefined).

- [ ] **Step 3: Implement**

In `internal/actions/executor.go`:

1. Constants and types:

```go
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
```

2. `Executor` gains:

```go
	// SelfNamespace and SelfName identify the agent's own pod, which a drain
	// never evicts. Both empty = unknown.
	SelfNamespace, SelfName string
```

3. `Execute` takes `progress ProgressFunc` and passes it to `drain` only: `res = e.drain(ctx, a, start, progress)`.

4. `InProgressDrains`:

```go
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
			dropped = append(dropped, name)
			_, _ = e.Kube.PatchNode(ctx, name, map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationDrainInProgress: nil}}})
			continue
		}
		actions = append(actions, protocol.Action{ActionID: st.ActionID, Type: protocol.ActionDrain, Params: protocol.Params{
			Node: name, TimeoutSeconds: st.TimeoutSeconds, DeleteEmptyDirData: st.DeleteEmptyDirData}})
	}
	return actions, dropped, nil
}
```

5. Rewrite `drain` (keep its classification rules and messages; the changes are the resume detection, the own pod, the annotation, progress and a single exit path after the cordon):

```go
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
		r := e.failed(start, "drain of %s stopped: %s; the node stays cordoned", node, strings.Join(blockers, "; "))
		r.Details = map[string]any{"blocking_pods": blockers}
		return e.finishDrain(ctx, a, node, r)
	}

	if !resumed {
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
	report := func(evicted, remaining int, blocked []kube.Pod) {
		if progress == nil {
			return
		}
		list := make([]map[string]string, 0, min(len(blocked), maxBlockedInProgress))
		for _, p := range blocked[:min(len(blocked), maxBlockedInProgress)] {
			list = append(list, map[string]string{"pod": podKey(p), "reason": "PodDisruptionBudget"})
		}
		msg := fmt.Sprintf("draining %s: %d pods evicted, %d remaining", node, evicted, remaining)
		if len(blocked) > 0 {
			msg += fmt.Sprintf(", %d blocked by a PodDisruptionBudget", len(blocked))
		}
		progress(protocol.Progress{UpdatedAt: e.Now().UTC(), Message: msg,
			Details: map[string]any{"evicted": evicted, "remaining": remaining, "blocked": list}})
	}
	report(0, len(toEvict), nil)
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
		report(len(toEvict)-len(pending), len(pending), pending)
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
		report(len(toEvict)-len(remaining), len(remaining), nil)
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
```

Notes:
- `drainTimeout` keeps its signature and message (it prints the total timeout).
- `e.succeeded` and `e.failed` are the existing helpers; check `e.succeeded`'s signature (`start, details, format, args...`) and adapt the call if it differs.
- `resultAnnotations` returns `map[string]any`, so adding a `nil` entry is valid.
- Imports: add `errors` and `encoding/json` if missing.
- `min` is the Go 1.21+ builtin.

- [ ] **Step 4: Run the tests and commit**

Run: `go test -race ./internal/actions/ ./internal/kubefake/ && go test -race ./... && go vet ./... && gofmt -l .`
Expected: PASS (existing drain tests included), nothing from gofmt.

```bash
git add internal/actions internal/kubefake internal/agent
git commit -m "Report drain progress, resume interrupted drains and skip the agent's pod

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Agent: progress reporter, resume at startup, own pod wiring, chart env

**Files:**
- Create: `internal/agent/progress.go`
- Modify: `internal/agent/agent.go`, `cmd/thumbops-agent/main.go`, `charts/thumbops-agent/templates/deployment.yaml`, `test/chart/check.sh`, `deploy/agent.yaml` (regenerated)
- Test: `internal/agent/progress_test.go` (new), `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: `actions.ProgressFunc`, `actions.ErrActionGone`, `(*actions.Executor).Execute(ctx, a, progress)`, `InProgressDrains`, `SelfNamespace`/`SelfName` (Task 3); `backend.Client.SendProgress`, mock `EnqueueClaimed`, `Progress`, `Cancel` (Task 2).
- Produces: `agent.Config.ProgressMinGap`, `agent.Config.ProgressMaxGap` (defaults 5 s, 30 s).

- [ ] **Step 1: Write the failing tests**

`internal/agent/progress_test.go`:

```go
package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/mockbackend"
	"github.com/thumbops/agent/internal/protocol"
)

func newReporterEnv(t *testing.T) (*mockbackend.Server, *backend.Client) {
	t.Helper()
	mb := mockbackend.New()
	srv := httptest.NewServer(mb.Handler())
	t.Cleanup(srv.Close)
	return mb, backend.New(backend.Options{BaseURL: srv.URL})
}

func TestReporterThrottles(t *testing.T) {
	mb, b := newReporterEnv(t)
	mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	now := time.Unix(1000, 0)
	_, cancel := context.WithCancelCause(context.Background())
	r := newProgressReporter(context.Background(), b, "d1", cancel, func() time.Time { return now },
		slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, 30*time.Second)

	p := func(msg string) protocol.Progress { return protocol.Progress{Message: msg} }
	r.report(p("a")) // first: sent at once
	now = now.Add(time.Second)
	r.report(p("b")) // changed but within 5 s: skipped
	now = now.Add(5 * time.Second)
	r.report(p("c")) // changed, 6 s later: sent
	now = now.Add(10 * time.Second)
	r.report(p("c")) // unchanged, 10 s later: skipped
	now = now.Add(25 * time.Second)
	r.report(p("c")) // unchanged, 35 s later: sent
	if last, n := mb.Progress("d1"); n != 3 || last.Message != "c" {
		t.Fatalf("sent %d, last %q; want 3 and c", n, last.Message)
	}
}

func TestReporterStopsTheActionOnGone(t *testing.T) {
	mb, b := newReporterEnv(t)
	mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	mb.Cancel("d1")
	ctx, cancel := context.WithCancelCause(context.Background())
	r := newProgressReporter(ctx, b, "d1", cancel, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, 30*time.Second)
	r.report(protocol.Progress{Message: "a"})
	if !errors.Is(context.Cause(ctx), actions.ErrActionGone) {
		t.Fatalf("a 410 must cancel the action with ErrActionGone, got %v", context.Cause(ctx))
	}
}

func TestReporterStopsReportingOn404(t *testing.T) {
	_, b := newReporterEnv(t) // action unknown to the backend: 404
	ctx, cancel := context.WithCancelCause(context.Background())
	now := time.Unix(1000, 0)
	r := newProgressReporter(ctx, b, "d1", cancel, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, 30*time.Second)
	r.report(protocol.Progress{Message: "a"})
	if ctx.Err() != nil {
		t.Fatal("a 404 must not stop the action")
	}
	if !r.disabled {
		t.Fatal("a 404 must stop further progress for the action")
	}
}
```

Append to `internal/agent/agent_test.go`:

```go
func TestDrainProgressReachesTheBackend(t *testing.T) {
	e := newEnv(t, nil, func(c *Config) { c.ProgressMinGap = time.Millisecond })
	e.fk.AddPod(podOn("payments", "api-1", "worker-1"))
	e.fk.AddPod(podOn("payments", "api-2", "worker-1"))
	e.fk.BlockEviction("payments", "api-2", 3)
	e.mb.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}})
	e.agent.handle(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}, ExpiresAt: time.Now().Add(time.Minute)})
	res, ok := e.mb.Result("d1")
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("result: %+v %v", res, ok)
	}
	if _, n := e.mb.Progress("d1"); n < 2 {
		t.Fatalf("expected progress reports, got %d", n)
	}
}

func TestDrainStoppedWhenTheBackendCancels(t *testing.T) {
	e := newEnv(t, nil)
	e.fk.AddPod(podOn("payments", "db-0", "worker-1"))
	e.fk.BlockEviction("payments", "db-0", -1)
	e.mb.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.agent.handle(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
			Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}, ExpiresAt: time.Now().Add(time.Minute)})
	}()
	waitUntil(t, func() bool { _, n := e.mb.Progress("d1"); return n >= 1 })
	e.mb.Cancel("d1")
	select {
	case <-done:
	case <-time.After(45 * time.Second): // the next progress is at most 30 s away
		t.Fatal("the drain did not stop after the backend cancelled it")
	}
	if _, ok := e.mb.Result("d1"); ok {
		t.Fatal("no result must be sent for a cancelled action")
	}
	if _, ok := e.fk.Node("worker-1").Metadata.Annotations[actions.AnnotationDrainInProgress]; ok {
		t.Fatal("the in-progress annotation must be removed")
	}
}

func TestInterruptedDrainIsResumedAtStartup(t *testing.T) {
	e := newEnv(t, nil)
	e.fk.AddPod(podOn("payments", "api-1", "worker-1"))
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().Add(-time.Second).UTC(), "timeout_seconds": 30})
	e.fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{actions.AnnotationDrainInProgress: string(state)}}})
	e.mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}})

	stop := e.run(t)
	defer stop()
	res, ok := e.mb.WaitResult("d1", 5*time.Second)
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("the resumed drain must report its result: %+v %v", res, ok)
	}
	if e.fk.PodExists("payments", "api-1") {
		t.Fatal("the resumed drain must evict the pod")
	}
}
```

`TestDrainStoppedWhenTheBackendCancels` waits for the reporter's 30 s maximum gap. Set `ProgressMaxGap: 100 * time.Millisecond` and `ProgressMinGap: time.Millisecond` in its `newEnv` option func so it finishes in well under a second, and reduce the timeout of the `select` to 5 s.

Helpers, if not already in the file (check `agent_test.go` first and reuse what exists):

```go
func podOn(ns, name, node string) kube.Pod {
	yes := true
	return kube.Pod{
		Metadata: kube.ObjectMeta{Name: name, Namespace: ns, UID: name,
			OwnerReferences: []kube.OwnerReference{{Kind: "ReplicaSet", Name: "rs", Controller: &yes}}},
		Spec:   kube.PodSpec{NodeName: node},
		Status: kube.PodStatus{Phase: "Running"},
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

Also make sure `newEnv` sets `exec.PollInterval` small (it already sets 10 ms) and imports `encoding/json`, `internal/kube`, `internal/actions`.

Run: `go test ./internal/agent/`
Expected: FAIL (build errors: `newProgressReporter`, `ProgressMinGap`, ...).

- [ ] **Step 2: The reporter**

`internal/agent/progress.go`:

```go
package agent

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/protocol"
)

// progressReporter sends the progress of one action: the first report at
// once, a change at most every minGap, an unchanged state at least every
// maxGap. It never retries. 409/410 cancel the action with
// actions.ErrActionGone; 404 (no progress support) stops the reports.
type progressReporter struct {
	ctx      context.Context
	backend  *backend.Client
	actionID string
	cancel   context.CancelCauseFunc
	now      func() time.Time
	log      *slog.Logger
	minGap   time.Duration
	maxGap   time.Duration

	last     time.Time
	lastMsg  string
	disabled bool
}

func newProgressReporter(ctx context.Context, b *backend.Client, actionID string, cancel context.CancelCauseFunc,
	now func() time.Time, log *slog.Logger, minGap, maxGap time.Duration) *progressReporter {
	return &progressReporter{ctx: ctx, backend: b, actionID: actionID, cancel: cancel, now: now, log: log, minGap: minGap, maxGap: maxGap}
}

// report is an actions.ProgressFunc; it runs on the action's goroutine.
func (r *progressReporter) report(p protocol.Progress) {
	if r.disabled {
		return
	}
	now := r.now()
	switch {
	case r.last.IsZero():
	case p.Message != r.lastMsg && now.Sub(r.last) >= r.minGap:
	case now.Sub(r.last) >= r.maxGap:
	default:
		return
	}
	r.last, r.lastMsg = now, p.Message
	err := r.backend.SendProgress(r.ctx, r.actionID, p)
	switch backend.Code(err) {
	case 0:
		if err != nil && r.ctx.Err() == nil {
			r.log.Warn("sending the progress failed", "err", err)
		}
	case http.StatusConflict, http.StatusGone:
		r.disabled = true
		r.log.Warn("the backend no longer tracks the action: stopping it", "code", backend.Code(err))
		r.cancel(actions.ErrActionGone)
	case http.StatusNotFound:
		r.disabled = true
		r.log.Info("the backend does not accept progress for this action: no more progress")
	default:
		r.log.Warn("sending the progress failed", "err", err)
	}
}
```

- [ ] **Step 3: Agent wiring**

In `internal/agent/agent.go`:

1. `Config` gains:

```go
	// Progress throttling (defaults 5s and 30s).
	ProgressMinGap time.Duration
	ProgressMaxGap time.Duration
```

with defaults set in `New` next to the others.

2. Split the execution out of `handle`. Replace the block that runs the action (from `log.Info("running action"...)` to the metrics after `Execute`) with a call to a new method, and skip the result when the backend stopped the action:

```go
	var res protocol.Result
	if err := a.checkPolicy(ctx, act); err != nil {
		// unchanged: rejected result, metrics, log
	} else {
		var gone bool
		if res, gone = a.execute(ctx, act, log); gone {
			return
		}
	}
```

and add:

```go
// execute runs a claimed action with a progress reporter. gone is true when
// the backend stopped tracking the action (409/410 on progress): no result
// must be sent.
func (a *Agent) execute(ctx context.Context, act protocol.Action, log *slog.Logger) (res protocol.Result, gone bool) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rep := newProgressReporter(actx, a.backend, act.ActionID, cancel, a.serverNow, log, a.cfg.ProgressMinGap, a.cfg.ProgressMaxGap)
	log.Info("running action", "params", act.Params, "requested_by", act.RequestedBy)
	a.cfg.Metrics.ActionRunning(true)
	start := time.Now()
	res = a.exec.Execute(actx, act, rep.report)
	a.cfg.Metrics.ObserveActionDuration(act.Type, time.Since(start))
	a.cfg.Metrics.ActionRunning(false)
	if errors.Is(context.Cause(actx), actions.ErrActionGone) {
		a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeDiscarded)
		log.Warn("action stopped: the backend no longer tracks it, no result sent")
		return res, true
	}
	a.cfg.Metrics.ActionOutcome(act.Type, res.Status)
	log.Info("action finished", "status", res.Status, "message", res.Message)
	return res, false
}
```

Keep `handle`'s existing structure otherwise (expired check, claim, policy, `sendResult`, `lastActionID`), and keep exactly the metrics it records today for the other paths.

3. Resume in `Run`: right after the heartbeat goroutine and the status loop are started (before the polling `for` loop), call `a.resumeDrains(ctx)`:

```go
// resumeDrains finishes the drains interrupted by a restart (nodes with the
// drain-in-progress annotation): no new claim, no policy check (both
// happened before the restart).
func (a *Agent) resumeDrains(ctx context.Context) {
	acts, dropped, err := a.exec.InProgressDrains(ctx)
	if err != nil {
		a.log.Warn("cannot look for interrupted drains", "err", err)
		return
	}
	for _, node := range dropped {
		a.log.Warn("unreadable drain-in-progress annotation removed; the node stays cordoned", "node", node)
	}
	for _, act := range acts {
		log := a.log.With("action_id", act.ActionID, "type", act.Type)
		log.Info("resuming an interrupted drain", "node", act.Params.Node)
		res, gone := a.execute(ctx, act, log)
		if gone {
			continue
		}
		a.sendResult(ctx, act.ActionID, res, log)
		a.mu.Lock()
		a.lastActionID = act.ActionID
		a.mu.Unlock()
	}
}
```

Imports: `errors`, `internal/actions`, `internal/metrics` as needed.

- [ ] **Step 4: The agent's own pod**

In `cmd/thumbops-agent/main.go`, where `actions.New(k)` is created, set the own pod:

```go
	exec := actions.New(k)
	exec.SelfNamespace, exec.SelfName = selfPod()
```

pass `exec` to `agent.New` instead of `actions.New(k)`, and add:

```go
// selfPod returns the agent's own pod, which a drain never evicts: from the
// downward API (POD_NAMESPACE, POD_NAME), else the ServiceAccount namespace
// and the hostname (the pod name by default).
func selfPod() (namespace, name string) {
	namespace, name = os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME")
	if namespace == "" {
		namespace, _ = kube.InClusterNamespace()
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	return namespace, name
}
```

In `charts/thumbops-agent/templates/deployment.yaml`, in the container, right after `imagePullPolicy`:

```yaml
          env:
            # The agent's own pod: a drain never evicts it.
            - name: POD_NAME
              valueFrom: { fieldRef: { fieldPath: metadata.name } }
            - name: POD_NAMESPACE
              valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
```

In `test/chart/check.sh`, in the assertions section, add:

```bash
grep -q 'fieldPath: metadata.name' "$out/defaults.yaml" || fail "POD_NAME downward API missing"
```

Regenerate: `hack/gen-manifest.sh && git add deploy/agent.yaml`.

- [ ] **Step 5: Run everything and commit**

Run: `go test -race ./... && go vet ./... && gofmt -l . && export PATH="$(go env GOPATH)/bin:$PATH" && test/chart/check.sh`
Expected: all pass; check.sh ends with `--- chart checks passed`.

```bash
git add internal/agent cmd/thumbops-agent charts test/chart deploy/agent.yaml
git commit -m "Send drain progress, stop on 409/410 and resume interrupted drains

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: End-to-end test and docs

**Files:**
- Modify: `test/e2e/run.sh`, `README.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: mock `/debug/actions` rows with `progress` and `progress_count` (Task 2); agent log line `"resuming an interrupted drain"` (Task 4); annotation `thumbops.mobiletechnologies.cloud/drain-in-progress` (Task 3).

- [ ] **Step 1: New e2e section**

In `test/e2e/run.sh`, before the final `[[ $(agent_restarts) == 0 ]]` line, add:

```bash
log "drain progress and resume after an agent restart"
kubectl create deployment slow --image=registry.k8s.io/pause:3.10 --replicas=1 --dry-run=client -o yaml |
  kubectl apply -f -
kubectl patch deployment slow --type merge -p '{"spec":{"template":{"spec":{"nodeSelector":{"thumbops-e2e":"workload"}}}}}'
kubectl uncordon "$workload_node"
kubectl rollout status deploy/slow --timeout=120s
kubectl create poddisruptionbudget slow --selector=app=slow --min-available=1
drain_id=$(backend POST /debug/actions "{\"type\":\"drain\",\"params\":{\"node\":\"$workload_node\",\"timeout_seconds\":240}}" | jq -r .action_id)

action_row() { backend GET /debug/actions | jq --arg id "$drain_id" '.[] | select(.action.action_id == $id)'; }
blocked_reported() { action_row | jq -e '.progress.details.blocked | length > 0' >/dev/null; }
wait_for "progress with the PDB-blocked pod" 60 blocked_reported
node_state() { kubectl get node "$workload_node" -o jsonpath='{.metadata.annotations.thumbops\.mobiletechnologies\.cloud/drain-in-progress}'; }
[[ $(node_state | jq -r .action_id) == "$drain_id" ]]

count_before=$(action_row | jq .progress_count)
old=$(kubectl -n thumbops get pods -l app.kubernetes.io/name=thumbops-agent -o name)
kubectl -n thumbops delete $old --wait=false
kubectl -n thumbops wait --for=delete $old --timeout=120s
wait_for "agent start" 60 agent_logged '"agent started"'
wait_for "drain resume" 60 agent_logged '"resuming an interrupted drain"'
more_progress() { (( $(action_row | jq .progress_count) > count_before )); }
wait_for "progress after the resume" 60 more_progress

kubectl delete poddisruptionbudget slow
wait_for "result of the resumed drain" 120 has_result "$drain_id"
[[ $(action_row | jq -r .result.status) == succeeded ]]
[[ -z $(node_state) ]]
kubectl delete deployment slow
```

Notes for the implementer:
- `workload_node`, `backend`, `wait_for`, `agent_logged`, `has_result` already exist in the script; read the whole script first.
- The uncordon is needed because the earlier sections leave `$workload_node` uncordoned already, but make it explicit so this section does not depend on their order.
- `kubectl delete $old --wait=false` then `wait --for=delete`: the agent receives SIGTERM, stops the drain without removing the annotation, and the new pod resumes it.
- Update the header comment: the scenario also covers drain progress and the resume of a drain after an agent restart.

- [ ] **Step 2: Run the end-to-end tests on kind (dedicated KUBECONFIG)**

```bash
export KUBECONFIG=<SCRATCH>/kind-kubeconfig
kind create cluster --name thumbops-e2e --config test/e2e/kind.yaml
test/e2e/run.sh
kind delete cluster --name thumbops-e2e
```

Expected: `--- end-to-end tests passed`. Always delete the cluster. Never use `~/.kube/config`.

- [ ] **Step 3: Docs**

README, "Prototype choices", replace the "Drain like `kubectl drain`" bullet with:

```markdown
- **Drain like `kubectl drain`**: it skips DaemonSet, static and terminated
  pods, and the agent's own pod (reported in the result: it moves at the
  agent's next restart); it stops *before* cordoning if it finds pods
  without a controller or with `emptyDir` volumes (the latter only with
  explicit consent); it uses the Eviction API, so it honors
  PodDisruptionBudgets, and retries blocked pods in rounds until the
  timeout. While it runs it sends progress (evicted, remaining and blocked
  pods), which also keeps the action alive in the backend. The cordon patch
  marks the node with `thumbops.mobiletechnologies.cloud/drain-in-progress`:
  an agent that restarts mid-drain resumes it with the time left.
```

and remove the "Intermediate results for long drains" line from "Missing for production" (if the section becomes empty, remove the section).

CLAUDE.md, invariant "Safe drain": append "; never evict the agent's own pod; the `drain-in-progress` annotation is written with the cordon and removed with the result (kept on shutdown, so the drain is resumed; removed on `409`/`410` to progress, which stop the drain without a result)". Add to the list of what `internal/agent` does: progress reporting and drain resume.

- [ ] **Step 4: Final checks and commit**

Run: `go test -race ./... && go vet ./... && gofmt -l . && test/chart/check.sh`

```bash
git add test/e2e/run.sh README.md CLAUDE.md
git commit -m "Check drain progress and resume end to end, document them

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
