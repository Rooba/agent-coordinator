package daemon

import (
	"encoding/json"
	"errors"
	"io"
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
	typ, taskBody := taskMessageType(req.Body)
	if !taskBody {
		return protocol.Response{}, false
	}
	to, ok := taskStates[typ]
	taskID, valid := decodeTaskReport(typ, req.Body)
	if !ok || !valid {
		return fail(errReservedTask), true
	}
	// Lifecycle mail always uses the structured cross-workspace send and the
	// one requester the daemon stamps on task.launch and task.cancel alike, so
	// a report answering either is aimed at the same place. The store still
	// routes from its ledger, but rejecting any other target catches malformed
	// or replayed broker frames instead of silently fixing them up.
	task, err := st.EyesTask(taskID)
	if err != nil {
		return protocol.Response{Error: relayError(err)}, true
	}
	if req.Op != protocol.OpSendWorkspace || req.Target == nil ||
		req.Target.Scope != task.RequesterScope || req.Target.AgentID != task.RequesterAgentID {
		return fail(errReservedTask), true
	}
	// Who may make this move is the store's call: only the broker holding the
	// task, or the child it minted for it, has standing in it.
	if _, _, err := st.TransitionEyesTask(taskID, eyesActor(req, actor), to, req.Body); err != nil {
		return protocol.Response{Error: relayError(err)}, true
	}
	return protocol.Response{OK: true, TaskID: taskID}, true
}

// taskMessageType reads only top-level fields, so nested or quoted task text
// remains ordinary mail. Once a top-level task.* type is seen, later malformed
// JSON still belongs to the reserved family and the strict typed decoder will
// refuse it. A repeated type is only the lifecycle's when one of its values
// names the family - otherwise it is odd JSON, and odd JSON is still mail.
func taskMessageType(body string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(body))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return "", false
	}
	typ := ""
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			break
		}
		if key != "type" {
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				break
			}
			continue
		}
		var next string
		if err := decoder.Decode(&next); err != nil {
			break
		}
		if seen && (strings.HasPrefix(typ, taskPrefix) || strings.HasPrefix(next, taskPrefix)) {
			return taskPrefix, true // reserved, and no one type to decode as
		}
		typ, seen = next, true
	}
	return typ, strings.HasPrefix(typ, taskPrefix)
}

// decodeTaskReport strictly decodes the one shared wire struct named by typ.
// Unknown fields, trailing values, type mismatches and absent required fields
// all make the reserved task body invalid rather than ordinary mail.
func decodeTaskReport(typ, body string) (string, bool) {
	validRef := func(ref *protocol.AgentRef) bool {
		return ref == nil || ref.Name != "" && ref.AgentID != "" && ref.Scope != ""
	}
	switch typ {
	case protocol.TaskAccepted:
		var msg protocol.TaskAcceptedMsg
		if !decodeTaskJSON(body, &msg) || msg.Type != typ || !validTaskID(msg.TaskID) || !validRef(msg.Child) {
			return "", false
		}
		return msg.TaskID, true
	case protocol.TaskResult:
		var msg protocol.TaskResultMsg
		if !decodeTaskJSON(body, &msg) || msg.Type != typ || !validTaskID(msg.TaskID) ||
			(msg.Status != "succeeded" && msg.Status != "failed") || strings.TrimSpace(msg.Summary) == "" ||
			msg.Observations == nil || msg.Actions == nil || msg.Evidence == nil ||
			(msg.Status == "succeeded" && msg.Error != "") ||
			(msg.Status == "failed" && strings.TrimSpace(msg.Error) == "") {
			return "", false
		}
		return msg.TaskID, true
	case protocol.TaskFailed:
		var msg protocol.TaskFailedMsg
		if !decodeTaskJSON(body, &msg) || msg.Type != typ || !validTaskID(msg.TaskID) || strings.TrimSpace(msg.Error) == "" {
			return "", false
		}
		return msg.TaskID, true
	}
	return "", false
}

func decodeTaskJSON(body string, dst any) bool {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst) == nil && decoder.Decode(new(any)) == io.EOF
}

func validTaskID(taskID string) bool {
	if len(taskID) != len("task-")+12 || !strings.HasPrefix(taskID, "task-") {
		return false
	}
	for _, c := range taskID[len("task-"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// pendingTaskMail is what this broker still owes an answer for: launches it
// has not acked, plus cancels it has not reported on. Redelivery is
// at-least-once on purpose - a broker that lost a poll must see the work
// again, and dropping a duplicate by task id is its job.
func pendingTaskMail(st *store.Store, sessionID string, broker bool) ([]protocol.Message, error) {
	// Broker is true only for the relay-authenticated launcher row dispatch
	// resolved. A wire-level kind claim has no path to this queue.
	if !broker {
		return nil, nil
	}
	launches, err := st.PendingLaunches(sessionID)
	if err != nil {
		return nil, err
	}
	cancels, err := st.PendingCancels(sessionID)
	if err != nil {
		return nil, err
	}
	return append(launches, cancels...), nil
}

// withPending folds that redelivery queue into a read or a peek reply, ahead
// of the fresh mail. It cannot duplicate anything: a broker's poll leaves the
// raw launch and cancel rows out, so the queue is the only place it ever sees
// them - and the only one that knows what is still owed.
func withPending(st *store.Store, req protocol.Request, broker bool, resp protocol.Response) protocol.Response {
	owed, err := pendingTaskMail(st, req.SessionID, broker)
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
