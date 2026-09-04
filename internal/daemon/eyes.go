package daemon

import (
	"encoding/json"
	"slices"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

// sendOps are the ops that carry a body, and so the ops a task report can
// arrive on. A broker replies to a launch like it replies to anything else.
var sendOps = map[string]bool{
	protocol.OpSend: true, protocol.OpBroadcast: true, protocol.OpSendWorkspace: true,
}

// taskStates maps a report body's type onto the lifecycle state it claims.
var taskStates = map[string]string{
	protocol.TaskAccepted: "accepted",
	protocol.TaskResult:   "done",
	protocol.TaskFailed:   "failed",
}

// requestEyes hands one brief to a live host broker. The requester is the
// authenticated actor, never a name the caller chose, so a report cannot be
// redirected; the store picks the broker, mints the id and queues the launch
// in one transaction.
func requestEyes(st *store.Store, req protocol.Request, actor protocol.AgentRef) protocol.Response {
	task, launcher, err := st.AssignEyesTask(store.EyesRequest{Requester: actor,
		Runtime: req.Runtime, Brief: req.Brief, DeadlineS: req.DeadlineS})
	if err != nil {
		return protocol.Response{Error: err.Error()}
	}
	return protocol.Response{OK: true, TaskID: task.TaskID, Launcher: &launcher}
}

// cancelEyes calls off a task the caller asked for. The store authorizes
// against the recorded requester and tells the broker in the same transaction,
// so a repeat is an ok no-op rather than a second task.cancel.
func cancelEyes(st *store.Store, req protocol.Request, actor protocol.AgentRef) protocol.Response {
	task, err := st.CancelEyesTask(req.TaskID, actor)
	if err != nil {
		return protocol.Response{Error: err.Error()}
	}
	return protocol.Response{OK: true, TaskID: task.TaskID}
}

// taskReport spots a lifecycle report inside outgoing mail: the launcher's ack
// and its child's outcome move the task instead of landing as ordinary mail,
// so the state change and the requester's copy commit together. Anything that
// is not a task body is left alone for the send it is.
func taskReport(st *store.Store, req protocol.Request) (protocol.Response, bool) {
	if !sendOps[req.Op] {
		return protocol.Response{}, false
	}
	var body struct {
		Type   string `json:"type"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		return protocol.Response{}, false
	}
	to, ok := taskStates[body.Type]
	if !ok {
		return protocol.Response{}, false
	}
	// Who may make this move is the store's call: only the broker holding the
	// task, or the child it minted for it, has standing in it.
	if _, _, err := st.TransitionEyesTask(body.TaskID, req.SessionID, to, req.Body); err != nil {
		return protocol.Response{Error: relayError(err)}, true
	}
	return protocol.Response{OK: true, TaskID: body.TaskID}, true
}

// pendingLaunches are the launches this caller was handed and has not acked.
// Redelivery is at-least-once on purpose: a broker that lost a poll must see
// the work again, and dropping a duplicate by task id is its job.
func pendingLaunches(st *store.Store, req protocol.Request) ([]protocol.Message, error) {
	if req.Kind != protocol.KindLauncher {
		return nil, nil // only a proven launcher row has launches to redeliver
	}
	return st.PendingLaunches(req.SessionID)
}

// prependLaunches puts the unacked launches first, without repeating one the
// same poll already returned.
func prependLaunches(launches, msgs []protocol.Message) []protocol.Message {
	if len(launches) == 0 {
		return msgs
	}
	fresh := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		fresh[m.ID] = true
	}
	out := make([]protocol.Message, 0, len(launches)+len(msgs))
	for _, l := range launches {
		if !fresh[l.ID] {
			out = append(out, l)
		}
	}
	return append(out, msgs...)
}

// mergeLaunches folds the unacked launches into a peek summary. They count as
// unread on every poll, read or not, because a broker long-polls with peek and
// must be woken about work it still owes an ack.
func mergeLaunches(launches []protocol.Message, info store.PeekInfo) store.PeekInfo {
	if len(launches) == 0 {
		return info // the ordinary poll, untouched
	}
	out := store.PeekInfo{HighWater: info.HighWater}
	for _, l := range launches {
		if slices.Contains(info.IDs, l.ID) {
			continue
		}
		out.IDs = append(out.IDs, l.ID)
		if !slices.Contains(out.Froms, l.From) {
			out.Froms = append(out.Froms, l.From)
		}
	}
	out.IDs = append(out.IDs, info.IDs...)
	for _, from := range info.Froms {
		if !slices.Contains(out.Froms, from) {
			out.Froms = append(out.Froms, from)
		}
	}
	out.Unread = len(out.IDs)
	return out
}
