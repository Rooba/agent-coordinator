package hookcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/daemon"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
	"github.com/Rooba/agent-coordinator/internal/store"
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
	// Pin PATH to a fake install so the hint names the bare program on any host.
	bin, exe := t.TempDir(), "agent-coordinator"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, exe), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
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

// Off PATH (a plugin install), the hint names this executable's absolute path.
func TestSessionStartWaitHintUsesExecutableOffPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sock, _ := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &out, sock)
	var env struct {
		HookSpecificOutput struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	ctx := env.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, exe) || !strings.Contains(ctx, " wait 'amber-fox' -timeout 570") {
		t.Fatalf("want wait hint naming %s: %s", exe, ctx)
	}
}

func TestSessionStartHonorsScopeAndKindEnv(t *testing.T) {
	t.Setenv("AC_SCOPE", "/home/ra/proj")
	t.Setenv("AC_KIND", protocol.KindEyes)
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	req := (*got)[0]
	if req.Scope != "/home/ra/proj" || req.Kind != protocol.KindEyes || req.Op != protocol.OpRegister {
		t.Fatalf("want AC_SCOPE/AC_KIND on register, got %+v", req)
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
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "vivid-owl/explore-1",
		Notices: []string{"[coordinator] 1 new message from amber-fox - call read_messages"}})
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
	for _, want := range []string{"additionalContext", "vivid-owl/explore-1", "from='vivid-owl/explore-1'", "amber-fox"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q: %s", want, out.String())
		}
	}
	if strings.Count(out.String(), "additionalContext") != 1 {
		t.Fatalf("identity and notices must share one hook response: %s", out.String())
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
	if !strings.Contains(out.String(), "additionalContext") || !strings.Contains(out.String(), "amber-fox/explore-1") {
		t.Fatalf("SubagentStart must introduce the child, got %s", out.String())
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
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"sessionId":"s1","cwd":"/x","hookEventName":"pre_tool_use","toolName":"Bash","toolInput":{"command":"ls"}}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("pre_tool_use for Bash must not deny: %s", out.String())
	}
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool != "Bash" || (*got)[0].Activity == "" || len((*got)[0].Writes) != 0 {
		t.Fatalf("pre_tool_use for Bash must record a start: %+v", *got)
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
	if out.Len() != 0 {
		t.Fatalf("parent read must not deny: %s", out.String())
	}
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool != "mcp__agent-coordinator__read_messages" {
		t.Fatalf("parent read must record a start: %+v", *got)
	}
}

func TestPreToolUseAllowsSubagentReadWithFrom(t *testing.T) {
	sock, got := fakeDaemonFunc(t, func(r protocol.Request) protocol.Response {
		if r.Op == protocol.OpWhoami {
			return protocol.Response{OK: true, Name: "quick-wolf"}
		}
		return protocol.Response{OK: true}
	})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"mcp__agent-coordinator__read_messages","tool_input":{"from":"quick-wolf/explore-1"},`+
		`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("read with own from must be a silent allow: %s", out.String())
	}
	if len(*got) != 2 || (*got)[0].Op != protocol.OpWhoami || (*got)[1].Op != protocol.OpEvent {
		t.Fatalf("want parent-name lookup then start: %+v", *got)
	}
}

// The deny must also catch the other names read_messages arrives under: the
// hookless alias, a use_tool wrapper, and the plugin-scoped MCP name.
func TestPreToolUseDeniesBareSubagentReadVariants(t *testing.T) {
	variants := map[string]string{
		"alias": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"agent-coordinator__read_messages","tool_input":{},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
		"use_tool": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"use_tool","tool_input":{"name":"read_messages","args":{}},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
		"plugin": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"mcp__plugin_agent-coordinator_agent-coordinator__read_messages","tool_input":{},` +
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
		"plugin": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"mcp__plugin_agent-coordinator_agent-coordinator__read_messages","tool_input":{"from":"quick-wolf/explore-1"},` +
			`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`,
	}
	for label, input := range variants {
		sock, got := fakeDaemonFunc(t, func(r protocol.Request) protocol.Response {
			if r.Op == protocol.OpWhoami {
				return protocol.Response{OK: true, Name: "quick-wolf"}
			}
			return protocol.Response{OK: true}
		})
		var out bytes.Buffer
		Run(strings.NewReader(input), &out, sock)
		if out.Len() != 0 {
			t.Fatalf("%s: read with own from must be a silent allow: %s", label, out.String())
		}
		if len(*got) != 2 || (*got)[0].Op != protocol.OpWhoami || (*got)[1].Op != protocol.OpEvent {
			t.Fatalf("%s: want parent-name lookup then start: %+v", label, *got)
		}
	}
}

// Only the coordinator's own server counts: another server's read_messages passes through.
func TestPreToolUseIgnoresOtherServerRead(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"mcp__other-server__read_messages","tool_input":{},`+
		`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("other server's read must not deny: %s", out.String())
	}
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent {
		t.Fatalf("other server's read must only record a start: %+v", *got)
	}
}

func TestPreToolUseIgnoresUnrelatedUseTool(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",`+
		`"tool_name":"use_tool","tool_input":{"name":"send_message","args":{}},`+
		`"agent_id":"a828b0d3d8ca1b28e","agent_type":"Explore"}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("unrelated use_tool must not deny: %s", out.String())
	}
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool != "send_message" {
		t.Fatalf("unrelated use_tool must record a start: %+v", *got)
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
		"plugin": `{"session_id":"parent-sess","cwd":"/x","hook_event_name":"PreToolUse",` +
			`"tool_name":"mcp__plugin_agent-coordinator_agent-coordinator__whoami","tool_input":{},` +
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
	if out.Len() != 0 {
		t.Fatalf("parent whoami must not deny: %s", out.String())
	}
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool != "mcp__agent-coordinator__whoami" {
		t.Fatalf("parent whoami must record a start: %+v", *got)
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

func TestCompactNotifiesPeersWithoutTaskClobber(t *testing.T) {
	for _, event := range []string{"PreCompact", "PostCompact"} {
		t.Run(event, func(t *testing.T) {
			sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "solid-orca"})
			var out bytes.Buffer
			Run(strings.NewReader(`{"session_id":"s1","cwd":"/x","hook_event_name":"`+event+`"}`), &out, sock)
			if out.Len() != 0 {
				t.Fatalf("compact must not emit hook output, got %s", out.String())
			}
			if len(*got) != 2 {
				t.Fatalf("want event+broadcast, got %+v", *got)
			}
			ev, bc := (*got)[0], (*got)[1]
			if ev.Op != protocol.OpEvent || ev.Tool != event || ev.Activity == "" || ev.ReplaceTasks || len(ev.Files) != 0 || len(ev.Tasks) != 0 {
				t.Fatalf("event = %+v", ev)
			}
			if bc.Op != protocol.OpBroadcast || !strings.Contains(bc.Body, "details may need restating") {
				t.Fatalf("broadcast = %+v", bc)
			}
		})
	}
}

func TestPermissionRequestAndInterruptRecordPresence(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"s1","cwd":"/x","hook_event_name":"PermissionRequest","tool_name":"Bash"}`), &out, sock)
	if out.Len() != 0 || len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool != "PermissionRequest" || (*got)[0].ReplaceTasks {
		t.Fatalf("PermissionRequest: out=%s req=%+v", out.String(), *got)
	}
	sock, got = fakeDaemon(t, protocol.Response{OK: true})
	out.Reset()
	Run(strings.NewReader(`{"session_id":"s1","cwd":"/x","hook_event_name":"Interrupt"}`), &out, sock)
	if out.Len() != 0 || len(*got) != 1 || (*got)[0].Op != protocol.OpIdle || (*got)[0].Activity != "" {
		t.Fatalf("Interrupt: out=%s req=%+v", out.String(), *got)
	}
}

func startHookDaemon(t *testing.T) string {
	t.Helper()
	dir := socktest.Dir(t)
	sock := filepath.Join(dir, "d.sock")
	st, err := store.Open(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = daemon.Serve(l, st, time.Minute); close(exited) }()
	t.Cleanup(func() { l.Close(); <-exited })
	return sock
}

func daemonReq(t *testing.T, sock string, req protocol.Request) protocol.Response {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("daemon %s: %+v", req.Op, resp)
	}
	return resp
}

func TestCompactBroadcastReachesLivePeersOnly(t *testing.T) {
	sock := startHookDaemon(t)
	t.Setenv("AC_NO_SPAWN", "1")
	scope, other := "/compact-peer", "/compact-other"
	a := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-a", Source: "hook"})
	b := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-b", Source: "hook"})
	c := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: other, SessionID: "sess-c", Source: "hook"})
	t.Setenv("AC_SCOPE", scope)
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"PreCompact"}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("compact hook output %s", out.String())
	}
	board := daemonReq(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: scope})
	var activity string
	for _, agent := range board.Agents {
		if agent.Name == a.Name {
			activity = agent.Activity
		}
	}
	if activity != "Compacting session" {
		t.Fatalf("compactor activity = %q agents=%+v", activity, board.Agents)
	}
	mailB := daemonReq(t, sock, protocol.Request{Op: protocol.OpRead, Scope: scope, SessionID: "sess-b", From: b.Name})
	if len(mailB.Messages) != 1 || mailB.Messages[0].From != a.Name || !strings.Contains(mailB.Messages[0].Body, "details may need restating") || !mailB.Messages[0].Broadcast {
		t.Fatalf("peer mail = %+v", mailB.Messages)
	}
	mailC := daemonReq(t, sock, protocol.Request{Op: protocol.OpRead, Scope: other, SessionID: "sess-c", From: c.Name})
	if len(mailC.Messages) != 0 {
		t.Fatalf("other scope got compact mail: %+v", mailC.Messages)
	}
}

func TestPresenceHooksPreservePendingNotices(t *testing.T) {
	sock := startHookDaemon(t)
	t.Setenv("AC_NO_SPAWN", "1")
	t.Setenv("AC_SCOPE", "/notice-keep")
	scope := "/notice-keep"
	a := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-a", Source: "hook"})
	b := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-b", Source: "hook"})
	daemonReq(t, sock, protocol.Request{Op: protocol.OpSend, Scope: scope, SessionID: "sess-b", From: b.Name, To: a.Name, Body: "ping"})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("PreToolUse consumed the notice: %s", out.String())
	}
	out.Reset()
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"Stop"}`), &out, sock)
	if !strings.Contains(out.String(), "1 new message") || !strings.Contains(out.String(), b.Name) {
		t.Fatalf("Stop should still see the notice: %s", out.String())
	}
}

func TestCompactChildDoesNotOverwriteParent(t *testing.T) {
	sock := startHookDaemon(t)
	t.Setenv("AC_NO_SPAWN", "1")
	t.Setenv("AC_SCOPE", "/compact-child")
	scope := "/compact-child"
	a := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-a", Source: "hook"})
	b := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-b", Source: "hook"})
	daemonReq(t, sock, protocol.Request{Op: protocol.OpEvent, Scope: scope, SessionID: "sess-a", Tool: "Read", Activity: "Reading x"})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"PreCompact","agent_id":"child1","agent_type":"Explore"}`), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("compact output %s", out.String())
	}
	board := daemonReq(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: scope})
	var parentAct, childAct, childName string
	for _, agent := range board.Agents {
		switch {
		case agent.Name == a.Name:
			parentAct = agent.Activity
		case agent.Parent == a.Name:
			childAct = agent.Activity
			childName = agent.Name
		}
	}
	if parentAct != "Reading x" {
		t.Fatalf("parent activity overwritten: %q board=%+v", parentAct, board.Agents)
	}
	if childAct != "Compacting session" || childName == "" {
		t.Fatalf("child compact missing: parent=%q child=%q/%q", parentAct, childName, childAct)
	}
	mailB := daemonReq(t, sock, protocol.Request{Op: protocol.OpRead, Scope: scope, SessionID: "sess-b", From: b.Name})
	if len(mailB.Messages) != 1 || mailB.Messages[0].From != childName {
		t.Fatalf("peer mail should be from child %s: %+v", childName, mailB.Messages)
	}
}

func TestFirstChildPreToolUseIntroducesWithoutEatingMail(t *testing.T) {
	sock := startHookDaemon(t)
	t.Setenv("AC_NO_SPAWN", "1")
	t.Setenv("AC_SCOPE", "/child-intro")
	scope := "/child-intro"
	a := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-a", Source: "hook"})
	b := daemonReq(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope, SessionID: "sess-b", Source: "hook"})
	daemonReq(t, sock, protocol.Request{Op: protocol.OpSend, Scope: scope, SessionID: "sess-b", From: b.Name, To: a.Name, Body: "ping"})
	var out bytes.Buffer
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"PreToolUse","agent_id":"child1","agent_type":"Explore","tool_name":"Bash","tool_input":{"command":"ls"}}`), &out, sock)
	if !strings.Contains(out.String(), "additionalContext") || !strings.Contains(out.String(), "you are") || !strings.Contains(out.String(), a.Name+"/") {
		t.Fatalf("first child PreToolUse must introduce: %s", out.String())
	}
	if strings.Contains(out.String(), "1 new message") {
		t.Fatalf("intro must not consume parent mail: %s", out.String())
	}
	out.Reset()
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"PostToolUse","agent_id":"child1","agent_type":"Explore","tool_name":"Bash","tool_input":{"command":"ls"}}`), &out, sock)
	if strings.Contains(out.String(), "you are") {
		t.Fatalf("later PostToolUse re-introduced the child: %s", out.String())
	}
	out.Reset()
	Run(strings.NewReader(`{"session_id":"sess-a","cwd":"/x","hook_event_name":"Stop"}`), &out, sock)
	if !strings.Contains(out.String(), "1 new message") || !strings.Contains(out.String(), b.Name) {
		t.Fatalf("parent Stop should still see the notice: %s", out.String())
	}
}
