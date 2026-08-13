package hookcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
)

func TestNormalizePaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cwd, "other", "b.go")
	got := normalizePaths(cwd, []string{filepath.Join("internal", "a.go"), abs})
	if len(got) != 2 || got[0] != filepath.Join(cwd, "internal", "a.go") || got[1] != abs {
		t.Fatalf("got %v", got)
	}
}

// fakeDaemonFunc answers each request via fn and records requests.
func fakeDaemonFunc(t *testing.T, fn func(protocol.Request) protocol.Response) (string, *[]protocol.Request) {
	t.Helper()
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var got []protocol.Request
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var r protocol.Request
			json.Unmarshal(line, &r)
			got = append(got, r)
			b, _ := json.Marshal(fn(r))
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock, &got
}

// fakeDaemon answers every request with the canned response and records requests.
func fakeDaemon(t *testing.T, resp protocol.Response) (string, *[]protocol.Request) {
	return fakeDaemonFunc(t, func(protocol.Request) protocol.Response { return resp })
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Skipf("fixture %s missing (run spike task 0): %v", name, err)
	}
	return b
}

func TestPostToolUseEmitsNotices(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Notices: []string{"[coordinator] 1 new message from amber-fox - call read_messages"}})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool == "" {
		t.Fatalf("daemon saw %+v", got)
	}
	if !strings.Contains(out.String(), "additionalContext") || !strings.Contains(out.String(), "amber-fox") {
		t.Fatalf("stdout: %s", out.String())
	}
}

func TestPostToolUseSilentWhenNoNotices(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("must emit nothing without notices, got %s", out.String())
	}
}

func TestSessionStartIntroducesName(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpRegister {
		t.Fatalf("daemon saw %+v", got)
	}
	if !strings.Contains(out.String(), "amber-fox") {
		t.Fatalf("stdout: %s", out.String())
	}
	// The injection teaches the wake pattern with the agent's actual name.
	if !strings.Contains(out.String(), "agent-coordinator wait 'amber-fox'") {
		t.Fatalf("missing wake-pattern teaching: %s", out.String())
	}
}

func TestStopEmitsBlockOnNotices(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true,
		Notices: []string{"[coordinator] 1 new message from amber-fox - call read_messages"}})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "stop.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpIdle {
		t.Fatalf("daemon saw %+v", got)
	}
	for _, want := range []string{`"decision"`, `"block"`, "amber-fox", "read_messages"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q: %s", want, out.String())
		}
	}
}

func TestStopSilentWithoutNotices(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "stop.json")), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("stop without notices must emit nothing, got %s", out.String())
	}
}

func TestUserPromptSubmitDrainsNotices(t *testing.T) {
	// No captured fixture exists for UserPromptSubmit, so the input is built
	// from the known common hook fields (session_id, cwd, hook_event_name)
	// plus this event's "prompt" field.
	const input = `{"session_id":"3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200","cwd":"/home/user/agent-coordinator-go","hook_event_name":"UserPromptSubmit","prompt":"please continue"}`
	sock, got := fakeDaemon(t, protocol.Response{OK: true,
		Notices: []string{"[coordinator] broadcast from brisk-owl - call read_messages"}})
	var out bytes.Buffer
	Run(strings.NewReader(input), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	if r := (*got)[0]; r.Op != protocol.OpEvent || r.Tool != "UserPromptSubmit" ||
		r.Activity != "Handling user prompt" || len(r.Files) != 0 || len(r.Writes) != 0 || r.TaskEv != nil {
		t.Fatalf("daemon saw %+v", r)
	}
	for _, want := range []string{`"UserPromptSubmit"`, "additionalContext", "brisk-owl"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q: %s", want, out.String())
		}
	}
}

func TestFailOpenWithoutDaemon(t *testing.T) {
	// AC_NO_SPAWN keeps the test hermetic - otherwise the miss would spawn the
	// test binary itself as "daemon".
	t.Setenv("AC_NO_SPAWN", "1")
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, filepath.Join(socktest.Dir(t), "absent.sock"))
	if out.Len() != 0 {
		t.Fatalf("must be silent when daemon is unreachable, got %s", out.String())
	}
}

// Subagent tool events carry the child identity fields so the daemon records
// the activity under the CHILD row (no more "(subagent: X)" tag on the parent).
func TestSubagentEventCarriesChildIdentity(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_bash_subagent.json")), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	r := (*got)[0]
	if r.Op != protocol.OpEvent || r.AgentID != "a828b0d3d8ca1b28e" || r.AgentType != "Explore" {
		t.Fatalf("daemon saw %+v", r)
	}
	if strings.Contains(r.Activity, "subagent") {
		t.Fatalf("activity must not carry the old parent-row tag: %q", r.Activity)
	}
}

func TestSubagentStartRegistersChild(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox/explore-1"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "subagent_start.json")), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	r := (*got)[0]
	if r.Op != protocol.OpRegister || r.Source != "hook-subagent" ||
		r.AgentID != "a828b0d3d8ca1b28e" || r.AgentType != "Explore" ||
		r.SessionID != "3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200" {
		t.Fatalf("daemon saw %+v", r)
	}
	if out.Len() != 0 {
		t.Fatalf("SubagentStart must emit nothing, got %s", out.String())
	}
}

func TestSubagentStopMarksChildGone(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "subagent_stop.json")), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	r := (*got)[0]
	if r.Op != protocol.OpDeregister || r.AgentID != "a828b0d3d8ca1b28e" ||
		r.SessionID != "3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200" {
		t.Fatalf("daemon saw %+v", r)
	}
}

// The Grok camelCase SessionStart envelope must yield the same registration
// request shape and the same injection line as the Claude snake_case one.
func TestGrokSessionStartMatchesClaude(t *testing.T) {
	sockC, gotC := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var outC bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &outC, sockC)

	sockG, gotG := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var outG bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start_grok.json")), &outG, sockG)

	if len(*gotC) != 1 || len(*gotG) != 1 {
		t.Fatalf("daemon saw claude=%+v grok=%+v", gotC, gotG)
	}
	c, g := (*gotC)[0], (*gotG)[0]
	if g.Op != protocol.OpRegister || g.Op != c.Op || g.Source != c.Source || g.SessionID != "abc-123" {
		t.Fatalf("grok register mismatch: claude=%+v grok=%+v", c, g)
	}
	if outG.String() != outC.String() {
		t.Fatalf("injection differs:\nclaude: %s\ngrok:   %s", outC.String(), outG.String())
	}
}

func TestGrokPostToolUseParsesCamelCase(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_bash_grok.json")), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	r := (*got)[0]
	if r.Op != protocol.OpEvent || r.Tool != "Bash" || r.SessionID != "abc-123" || r.Activity == "" {
		t.Fatalf("daemon saw %+v", r)
	}
}

// Event-name variants (session_start, sessionStart, pre_tool_use, ...) must
// fold onto the canonical events.
func TestEventNameVariantNormalization(t *testing.T) {
	for _, name := range []string{"session_start", "sessionStart"} {
		sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
		var out bytes.Buffer
		Run(strings.NewReader(`{"sessionId":"s1","cwd":"/x","hookEventName":"`+name+`"}`), &out, sock)
		if len(*got) != 1 || (*got)[0].Op != protocol.OpRegister {
			t.Fatalf("%s: daemon saw %+v", name, got)
		}
		if !strings.Contains(out.String(), "amber-fox") {
			t.Fatalf("%s: stdout %s", name, out.String())
		}
	}
	// pre_tool_use for an ordinary tool: allowed, no round trip, no output.
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"sessionId":"s1","cwd":"/x","hookEventName":"pre_tool_use","toolName":"Bash","toolInput":{"command":"ls"}}`), &out, sock)
	if len(*got) != 0 || out.Len() != 0 {
		t.Fatalf("pre_tool_use for Bash must be a silent allow: got=%+v out=%s", got, out.String())
	}
}

const bareSubagentRead = `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
	`"tool_name":"mcp__agent-coordinator__read_messages","tool_input":{},` +
	`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`

func TestPreToolUseDeniesBareSubagentRead(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "quick-wolf/explore-1"})
	var out bytes.Buffer
	Run(strings.NewReader(bareSubagentRead), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	if r := (*got)[0]; r.Op != protocol.OpRegister || r.Source != "hook-subagent" ||
		r.AgentID != "a828b0d3d8ca1b28e" || r.AgentType != "Explore" {
		t.Fatalf("child resolution request: %+v", r)
	}
	for _, want := range []string{`"permissionDecision":"deny"`, `"hookEventName":"PreToolUse"`,
		"from='quick-wolf/explore-1'", "Parent mail stays with the parent."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deny output missing %q: %s", want, out.String())
		}
	}
}

func TestPreToolUseAllowsParentRead(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"mcp__agent-coordinator__read_messages","tool_input":{}}`), &out, sock)
	if len(*got) != 0 || out.Len() != 0 {
		t.Fatalf("parent read must be a silent allow: got=%+v out=%s", got, out.String())
	}
}

func TestPreToolUseAllowsSubagentReadWithFrom(t *testing.T) {
	sock, got := fakeDaemonFunc(t, func(protocol.Request) protocol.Response {
		return protocol.Response{OK: true, Name: "quick-wolf"} // the parent's name
	})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"mcp__agent-coordinator__read_messages","tool_input":{"from":"quick-wolf/explore-1"},`+
		`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("read with own from must be a silent allow: %s", out.String())
	}
	if len(*got) != 1 || (*got)[0].Op != protocol.OpWhoami || (*got)[0].SessionID != "parent-sess" {
		t.Fatalf("want a single parent-name lookup: %+v", *got)
	}
}

// The deny must also catch the tool-name variants foreign harnesses present:
// the hookless alias and a generic use_tool wrapper naming read_messages.
func TestPreToolUseDeniesBareSubagentReadVariants(t *testing.T) {
	variants := map[string]string{
		"alias": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"agent-coordinator__read_messages","tool_input":{},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
		"use_tool": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"use_tool","tool_input":{"name":"read_messages","args":{}},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
	}
	for label, input := range variants {
		sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "quick-wolf/explore-1"})
		var out bytes.Buffer
		Run(strings.NewReader(input), &out, sock)
		if len(*got) != 1 || (*got)[0].Source != "hook-subagent" {
			t.Fatalf("%s: daemon saw %+v", label, got)
		}
		if !strings.Contains(out.String(), `"permissionDecision":"deny"`) ||
			!strings.Contains(out.String(), "from='quick-wolf/explore-1'") {
			t.Fatalf("%s: deny output: %s", label, out.String())
		}
	}
}

func TestPreToolUseAllowsVariantsWithFrom(t *testing.T) {
	variants := map[string]string{
		"alias": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"agent-coordinator__read_messages","tool_input":{"from":"quick-wolf/explore-1"},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
		"use_tool nested args": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"use_tool","tool_input":{"name":"read_messages","args":{"from":"quick-wolf/explore-1"}},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
	}
	for label, input := range variants {
		sock, got := fakeDaemonFunc(t, func(protocol.Request) protocol.Response {
			return protocol.Response{OK: true, Name: "quick-wolf"} // the parent's name
		})
		var out bytes.Buffer
		Run(strings.NewReader(input), &out, sock)
		if out.Len() != 0 {
			t.Fatalf("%s: read with own from must be a silent allow: %s", label, out.String())
		}
		if len(*got) != 1 || (*got)[0].Op != protocol.OpWhoami {
			t.Fatalf("%s: want only a parent-name lookup: %+v", label, *got)
		}
	}
}

// use_tool calls for unrelated tools are never touched.
func TestPreToolUseIgnoresUnrelatedUseTool(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"use_tool","tool_input":{"name":"send_message","args":{}},`+
		`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`), &out, sock)
	if len(*got) != 0 || out.Len() != 0 {
		t.Fatalf("unrelated use_tool must be a silent allow: got=%+v out=%s", got, out.String())
	}
}

// Fail-open: if the child name cannot be resolved (daemon error or daemon
// down), the call is allowed rather than blocked.
func TestPreToolUseFailsOpenWhenChildUnresolvable(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{Error: "boom"})
	var out bytes.Buffer
	Run(strings.NewReader(bareSubagentRead), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("daemon error must fail open, got %s", out.String())
	}
	t.Setenv("AC_NO_SPAWN", "1")
	out.Reset()
	Run(strings.NewReader(bareSubagentRead), &out, filepath.Join(socktest.Dir(t), "absent.sock"))
	if out.Len() != 0 {
		t.Fatalf("unreachable daemon must fail open, got %s", out.String())
	}
}

const subagentWhoami = `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
	`"tool_name":"mcp__agent-coordinator__whoami","tool_input":{},` +
	`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`

// Subagent whoami reports the PARENT identity over the shared connection, so
// it is denied with a reason that teaches the child its own name.
func TestPreToolUseDeniesSubagentWhoami(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "quick-wolf/explore-1"})
	var out bytes.Buffer
	Run(strings.NewReader(subagentWhoami), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpRegister || (*got)[0].Source != "hook-subagent" {
		t.Fatalf("child resolution request: %+v", *got)
	}
	for _, want := range []string{`"permissionDecision":"deny"`,
		"you are 'quick-wolf/explore-1' in this workspace",
		"use from='quick-wolf/explore-1' on coordinator tools",
		"whoami on this shared connection reports the parent identity."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deny output missing %q: %s", want, out.String())
		}
	}
}

func TestPreToolUseDeniesSubagentWhoamiVariants(t *testing.T) {
	variants := map[string]string{
		"alias": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"agent-coordinator__whoami","tool_input":{},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
		"use_tool": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"use_tool","tool_input":{"name":"whoami"},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
		"use_tool mcp name": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"use_tool","tool_input":{"name":"mcp__agent-coordinator__whoami"},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
	}
	for label, input := range variants {
		sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "quick-wolf/explore-1"})
		var out bytes.Buffer
		Run(strings.NewReader(input), &out, sock)
		if len(*got) != 1 || (*got)[0].Source != "hook-subagent" {
			t.Fatalf("%s: daemon saw %+v", label, *got)
		}
		if !strings.Contains(out.String(), `"permissionDecision":"deny"`) ||
			!strings.Contains(out.String(), "you are 'quick-wolf/explore-1'") {
			t.Fatalf("%s: deny output: %s", label, out.String())
		}
	}
}

func TestPreToolUseAllowsParentWhoami(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"mcp__agent-coordinator__whoami","tool_input":{}}`), &out, sock)
	if len(*got) != 0 || out.Len() != 0 {
		t.Fatalf("parent whoami must be a silent allow: got=%+v out=%s", *got, out.String())
	}
}

const readAsParent = `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
	`"tool_name":"mcp__agent-coordinator__read_messages","tool_input":{"from":"quick-wolf"},` +
	`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`

// A subagent naming the PARENT as from would drain the parent inbox: denied
// with the child-name teaching reason.
func TestPreToolUseDeniesSubagentReadAsParent(t *testing.T) {
	sock, got := fakeDaemonFunc(t, func(r protocol.Request) protocol.Response {
		if r.Op == protocol.OpWhoami {
			return protocol.Response{OK: true, Name: "quick-wolf"}
		}
		return protocol.Response{OK: true, Name: "quick-wolf/explore-1"}
	})
	var out bytes.Buffer
	Run(strings.NewReader(readAsParent), &out, sock)
	if len(*got) != 2 || (*got)[0].Op != protocol.OpWhoami || (*got)[0].SessionID != "parent-sess" ||
		(*got)[1].Op != protocol.OpRegister || (*got)[1].Source != "hook-subagent" {
		t.Fatalf("want parent lookup then child resolution: %+v", *got)
	}
	for _, want := range []string{`"permissionDecision":"deny"`,
		"you are 'quick-wolf/explore-1' in this workspace",
		"use from='quick-wolf/explore-1' on coordinator tools",
		"from='quick-wolf' is the parent's inbox"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deny output missing %q: %s", want, out.String())
		}
	}
}

// Fail-open for the new paths: an unresolvable parent name allows the
// from-bearing read, and an unresolvable child name allows whoami.
func TestPreToolUseNewDenyPathsFailOpen(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{Error: "boom"})
	var out bytes.Buffer
	Run(strings.NewReader(readAsParent), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("parent-name error must fail open, got %s", out.String())
	}
	out.Reset()
	Run(strings.NewReader(subagentWhoami), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("child-name error must fail open, got %s", out.String())
	}
	t.Setenv("AC_NO_SPAWN", "1")
	for _, input := range []string{readAsParent, subagentWhoami} {
		out.Reset()
		Run(strings.NewReader(input), &out, filepath.Join(socktest.Dir(t), "absent.sock"))
		if out.Len() != 0 {
			t.Fatalf("unreachable daemon must fail open, got %s", out.String())
		}
	}
}

func TestGarbageInputIsSilent(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader("not json at all"), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("garbage must be swallowed, got %s", out.String())
	}
}
