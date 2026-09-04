package daemon

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

// sendOps are the ops that carry a body: the ops a task report can arrive on,
// and the ops whose task id the relay gate strips. A broker replies to a
// launch like it replies to anything else.
var sendOps = map[string]bool{
	protocol.OpSend: true, protocol.OpBroadcast: true, protocol.OpSendWorkspace: true,
}

// taskStates maps a report body's type onto the lifecycle state it claims.
var taskStates = map[string]string{
	protocol.TaskAccepted: "accepted",
	protocol.TaskResult:   "done",
	protocol.TaskFailed:   "failed",
}

// taskPrefix marks the reserved message family: every task.* body belongs to
// the eyes lifecycle, so none of them may travel as ordinary mail.
const taskPrefix = "task."

// errReservedTask answers a task.* body this daemon will not act on: the
// launch and the cancel are its own to write, and an unknown task.* type is
// nothing it can honour.
var errReservedTask = errors.New("reserved task message")

// requestEyes hands one brief to a live host broker. The requester is the
// authenticated actor, never a name the caller chose, so a report cannot be
// redirected; the store picks the broker, mints the id and queues the launch
// in one transaction.
func requestEyes(st *store.Store, req protocol.Request, actor protocol.AgentRef) protocol.Response {
	task, launcher, err := st.AssignEyesTask(store.EyesRequest{Requester: actor,
		Runtime: req.Runtime, Brief: req.Brief, DeadlineS: req.DeadlineS})
	if err != nil {
		return fail(err)
	}
	return protocol.Response{OK: true, TaskID: task.TaskID, Launcher: &launcher}
}

// eyesActor is the authenticated caller behind a lifecycle move: the row this
// request proved, the ref its workspace knows it by, and the provenance the
// daemon stamped. Nothing here comes from the client's own claims.
func eyesActor(req protocol.Request, actor protocol.AgentRef) store.EyesActor {
	return store.EyesActor{Scope: req.Scope, SessionID: req.SessionID, Origin: req.Origin, Ref: actor}
}

// cancelEyes calls off a task the caller asked for. The store authorizes
// against the recorded requester and tells the broker in the same transaction,
// so a repeat is an ok no-op rather than a second task.cancel.
func cancelEyes(st *store.Store, req protocol.Request, actor protocol.AgentRef) protocol.Response {
	task, err := st.CancelEyesTask(req.TaskID, eyesActor(req, actor))
	if err != nil {
		return fail(err)
	}
	return protocol.Response{OK: true, TaskID: task.TaskID}
}

// taskReport spots a lifecycle report inside outgoing mail: the launcher's ack
// and its child's outcome move the task instead of landing as ordinary mail,
// so the state change and the requester's copy commit together. Anything that
// is not a task body is left alone for the send it is.
func taskReport(st *store.Store, req protocol.Request, actor protocol.AgentRef) (protocol.Response, bool) {
	if !sendOps[req.Op] {
		return protocol.Response{}, false
	}
	// The type alone decides whether this is the lifecycle's; only then does
	// the rest of the body have to be well formed, and a task.* body that is
	// not a report - or does not decode - is refused rather than delivered.
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(req.Body), &head) != nil || !strings.HasPrefix(head.Type, taskPrefix) {
		return protocol.Response{}, false
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	to, ok := taskStates[head.Type]
	if !ok || json.Unmarshal([]byte(req.Body), &body) != nil {
		return fail(errReservedTask), true
	}
	// Who may make this move is the store's call: only the broker holding the
	// task, or the child it minted for it, has standing in it.
	if _, _, err := st.TransitionEyesTask(body.TaskID, eyesActor(req, actor), to, req.Body); err != nil {
		return protocol.Response{Error: relayError(err)}, true
	}
	return protocol.Response{OK: true, TaskID: body.TaskID}, true
}

// pendingTaskMail is what this broker still owes an answer for: launches it
// has not acked, plus cancels it has not reported on. Redelivery is
// at-least-once on purpose - a broker that lost a poll must see the work
// again, and dropping a duplicate by task id is its job.
func pendingTaskMail(st *store.Store, req protocol.Request) ([]protocol.Message, error) {
	// Both queues are scoped to the tasks assigned to this session id. Over
	// the relay the gate has already proved that session; on the unix socket
	// the caller supplies it, where the socket's permissions are the trust
	// boundary as they are for every other op.
	if req.Kind != protocol.KindLauncher {
		return nil, nil
	}
	launches, err := st.PendingLaunches(req.SessionID)
	if err != nil {
		return nil, err
	}
	cancels, err := st.PendingCancels(req.SessionID)
	if err != nil {
		return nil, err
	}
	return append(launches, cancels...), nil
}

// withPending folds that redelivery queue into a read or a peek reply, ahead
// of the fresh mail. It cannot duplicate anything: a broker's poll leaves the
// raw launch and cancel rows out, so the queue is the only place it ever sees
// them - and the only one that knows what is still owed.
func withPending(st *store.Store, req protocol.Request, resp protocol.Response) protocol.Response {
	owed, err := pendingTaskMail(st, req)
	if err != nil {
		return fail(err)
	}
	if len(owed) == 0 {
		return resp // the ordinary poll, untouched
	}
	if req.Op == protocol.OpRead {
		resp.Messages = append(owed, resp.Messages...)
		return resp
	}
	// Owed work counts as unread on every peek, read or not, because a broker
	// long-polls with peek and must be woken about what it still owes.
	ids := make([]int64, 0, len(owed)+len(resp.PeekIDs))
	for _, m := range owed {
		ids = append(ids, m.ID)
		if !slices.Contains(resp.PeekFroms, m.From) {
			resp.PeekFroms = append(resp.PeekFroms, m.From)
		}
	}
	resp.PeekIDs = append(ids, resp.PeekIDs...)
	resp.Unread = len(resp.PeekIDs)
	return resp
}
