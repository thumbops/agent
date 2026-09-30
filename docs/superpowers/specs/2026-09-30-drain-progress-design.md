# Progress for long actions, drain resume and the agent's own pod

Date: 2026-09-30. Status: approved design, to be implemented.

Closes the protocol's open question "A drain can take minutes: is an
intermediate result (`progress`) needed, or is a longer timeout for that
action type enough?" The answer is: progress is needed.

## Problem

- The backend expires a `claimed` action without a result after 5 minutes,
  but a drain's default `timeout_seconds` is 600. A default drain can
  outlive its action: the backend marks it `expired`, the agent's result
  then gets `409`/`410`, the retries stop and the result is lost, while the
  node is drained or left cordoned with pods on it.
- If the agent restarts during a drain it does not know it had a claimed
  action: the node stays cordoned half drained until the backend expires the
  action.
- Draining the node the agent runs on evicts the agent itself mid-drain.
- The dashboard can only show "running" for minutes, with no hint of what
  blocks the drain (typically a PodDisruptionBudget).

## Protocol (thumbops/spec, protocol v1, compatible addition)

### `POST /v1/agent/actions/{id}/progress`

mTLS like the other `/v1/agent/*` calls. Body:

```json
{
  "updated_at": "2026-09-30T10:03:12Z",
  "message": "draining worker-3: 14 pods evicted, 2 remaining",
  "details": {
    "evicted": 14,
    "remaining": 2,
    "blocked": [
      { "pod": "payments/api-7f9c", "reason": "PodDisruptionBudget" }
    ]
  }
}
```

- `details` is free-form per action type; for `drain` it has `evicted`,
  `remaining` and `blocked` (at most 20 entries, `pod` as
  `namespace/name`, `reason` a short text).
- Sent for long actions only (today `drain`): right after the cordon, then
  whenever the counts change (at most once every 5 s), and at least every
  30 s.
- Responses:
  - `200` (empty body): the backend stores it for the dashboard and renews
    the action's lease (below).
  - `409`/`410`: the backend no longer tracks the action (expired or
    cancelled). The agent stops the action (no further evictions), leaves
    the node cordoned, removes the in-progress annotation and sends no
    result.
  - `404`: the backend does not know the endpoint (older backend) or the
    action. The agent stops sending progress for that action and goes on.
  - Network errors, `429`, `5xx`: not retried (the next progress is newer);
    the action goes on.

### Lease of a claimed action

The state table changes from "claimed without a result within 5 minutes"
to: a `claimed` action becomes `expired` when 5 minutes pass without a
result **and** without progress. Each progress renews the lease. A drain
whose agent died is noticed within 5 minutes of its last progress; a drain
that keeps making progress never expires.

### Resume after an agent restart

After a restart the agent may send progress and the result for an action it
claimed before, without a new claim. The backend accepts both while the
action is `claimed`, as usual.

### Drain of the agent's own node

The agent never evicts its own pod: it is skipped like DaemonSet pods and
reported in the result (`details.agent_pod_left`). The node is cordoned, so
the agent moves elsewhere at its next restart.

### Spec documents

- `protocol.md`: new "Progress" subsection in "Execution and results";
  updated state table; the drain parameter row mentions the agent's own
  pod; "Idempotency" describes the in-progress annotation and resume; the
  open question is removed.
- `openapi.yaml`: the new path and a `Progress` schema; `validate.py`
  passes.

## Agent

### Drain state on the node

- The cordon becomes a single merge patch setting `spec.unschedulable: true`
  and the annotation `thumbops.mobiletechnologies.cloud/drain-in-progress`,
  whose value is JSON:
  `{"action_id": "...", "started_at": "RFC 3339", "timeout_seconds": 600,
  "delete_emptydir_data": false}` (`started_at` on the agent's clock).
- The final patch (succeeded, failed after the cordon, timeout) removes the
  annotation (`null` in the merge patch) and writes `last-action-id` and
  `last-action-result`, in one patch. The in-progress annotation is also
  removed, alone, when the drain stops because of a `409`/`410` on
  progress.
- A drain that fails before cordoning (blockers) touches nothing, as today.

### Resume

- In `Run`, after the first heartbeat and before polling for new actions,
  the agent lists the nodes and, for each one with `drain-in-progress`,
  resumes the drain synchronously.
- Resume skips the claim and the local policy check (both happened before
  the restart), recomputes the pods to evict, and uses as timeout
  `timeout_seconds - (now - started_at)`. If that is not positive it
  finishes at once with the timeout result listing the pods still on the
  node.
- It sends progress and the result for the stored `action_id` like a normal
  drain. A `409`/`410` stops it (annotation removed, node cordoned); a
  result rejected with `400`/`409`/`410` is logged, as today.
- An unreadable annotation is logged, removed, and the node left cordoned.

### The agent's own pod

- The chart passes `POD_NAME` and `POD_NAMESPACE` with the downward API.
  Without them the agent falls back to the hostname and the ServiceAccount
  namespace.
- The drain skips that pod (`skipped["agent"]`), and the result has
  `details.agent_pod_left: "<namespace>/<name>"` and a sentence in the
  message.

### Progress reporting

- `protocol.Progress{UpdatedAt, Message, Details}`;
  `backend.Client.SendProgress(ctx, actionID, Progress) error`; the metrics
  operation `progress`.
- The executor takes a progress callback per execution and calls it after
  the cordon and after every eviction or wait round with the current
  counts and blocked pods (reason `PodDisruptionBudget` for a `429` on
  eviction).
- The agent wraps the callback in a reporter that applies the 5 s / 30 s
  rules, sends through the backend client, and on `409`/`410` cancels the
  action's context; on `404` it stops sending for that action. The drain
  treats a cancellation from the reporter like the protocol says (stop,
  remove the annotation, no result).

### Mock backend

- `POST /v1/agent/actions/{id}/progress`: `200` for a claimed action (stores
  the last progress and a count), `410` otherwise.
- Claimed actions expire 5 minutes after the claim or the last progress.
- `/debug/actions` shows the last progress and the progress count;
  `POST /debug/actions/{id}/cancel` makes a claimed action cancelled (next
  progress gets `410`).

## Tests

- Executor (kubefake):
  - progress calls with a PDB-blocked pod (`blocked` with the reason), then
    completion;
  - the in-progress annotation is written in the cordon patch and removed
    in the final one, on success and on timeout;
  - the agent's own pod is skipped and reported;
  - resume with time left completes; resume with no time left returns the
    timeout result; an unreadable annotation is removed.
- Agent (mock backend):
  - a drain in progress at startup (annotation present) is resumed without
    a claim and its result reaches the backend;
  - `410` on progress stops the evictions, removes the annotation and sends
    no result;
  - `404` on progress: the drain completes and the result is sent;
  - the reporter sends the first progress at once, throttles changes to
    one every 5 s and sends at least every 30 s (fake clock).
- End-to-end on kind:
  - a deployment on the workload worker with a PDB that allows no
    disruption; a drain starts and the mock backend records progress with
    the blocked pod;
  - the agent pod is deleted mid-drain; the new pod logs the resume and
    keeps sending progress for the same `action_id`;
  - the PDB is removed; the drain ends `succeeded`; the node has no
    `drain-in-progress` annotation.

## Out of scope

- Cancelling a running drain from the dashboard (the backend may answer
  `410` to progress for a cancelled action, and the agent honours it, but
  the app feature is the backend's).
- Progress for other action types (none is long today).
- Rollout status after `rollout-restart`.
