package hostrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	streamHelperTurns  = 5
	streamHelperTool   = "mcp__claude-in-chrome__navigate"
	streamHelperSecret = "SECRETPAGE"
)

// streamProvider runs the helper subprocess through the real Claude stream
// decoder and the real report parser, so the test exercises the shipped path
// rather than a stand-in for it.
type streamProvider struct {
	mode   string
	stream bool
}

func (p *streamProvider) Name() string { return "fake" }

func (p *streamProvider) Prepare(Task, string) (Invocation, error) {
	executable, err := os.Executable()
	if err != nil {
		return Invocation{}, err
	}
	scratch, err := os.Getwd()
	if err != nil {
		return Invocation{}, err
	}
	invocation := Invocation{
		Executable: executable,
		Args:       []string{"-test.run=^TestHostrunnerHelperProcess$", "--", p.mode, "", "", "", ""},
		Dir:        scratch,
		Prompt:     []byte("brief"),
		Decode:     parseClaudeReport,
	}
	if p.stream {
		invocation.Stream = claudeStreamEvent
	} else {
		invocation.ReportFromStdout = true
	}
	return invocation, nil
}

type progressRecorder struct {
	mu      sync.Mutex
	samples []Progress
}

func (r *progressRecorder) record(sample Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, sample)
}

func (r *progressRecorder) last() (Progress, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return Progress{}, 0
	}
	return r.samples[len(r.samples)-1], len(r.samples)
}

func newStreamRunner(t *testing.T, provider Provider, outputLimit int) *Runner {
	t.Helper()
	registry, err := NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(registry, Options{DefaultTimeout: MinTaskTimeout,
		OutputLimit: outputLimit, TempDir: t.TempDir(), waitDelay: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestRunnerStreamsProgressAndParsesFinalReport(t *testing.T) {
	recorder := &progressRecorder{}
	runner := newStreamRunner(t, &streamProvider{mode: "stream", stream: true}, 64<<10)
	result, err := runner.Run(context.Background(), helperTask(20), recorder.record)
	if err != nil {
		t.Fatal(err)
	}
	if result.Report.Status != ReportSucceeded || result.Report.Summary == "" {
		t.Fatalf("final report did not parse from the stream: %+v", result.Report)
	}
	last, count := recorder.last()
	if count == 0 {
		t.Fatal("no progress samples were published")
	}
	if last.Turns != streamHelperTurns || last.Tool != streamHelperTool {
		t.Fatalf("last sample = %+v, want %d turns of %s", last, streamHelperTurns, streamHelperTool)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for _, sample := range recorder.samples {
		if strings.Contains(sample.Tool, streamHelperSecret) {
			t.Fatalf("tool arguments or page content leaked into progress: %+v", sample)
		}
	}
}

// The fallback keeps the one output mode known to work end to end on the host:
// a single blob at exit, no progress, and the same report.
func TestRunnerSingleBlobFallbackStillReports(t *testing.T) {
	recorder := &progressRecorder{}
	runner := newStreamRunner(t, &streamProvider{mode: "blob"}, 64<<10)
	result, err := runner.Run(context.Background(), helperTask(21), recorder.record)
	if err != nil {
		t.Fatal(err)
	}
	if result.Report.Status != ReportSucceeded {
		t.Fatalf("blob report did not parse: %+v", result.Report)
	}
	if _, count := recorder.last(); count != 0 {
		t.Fatalf("blob mode published %d progress samples", count)
	}
}

// A stream longer than the diagnostic buffer must still yield its report: the
// reader sees every byte even after the buffer caps out.
func TestRunnerStreamSurvivesTruncatedStdout(t *testing.T) {
	runner := newStreamRunner(t, &streamProvider{mode: "stream", stream: true}, 128)
	result, err := runner.Run(context.Background(), helperTask(22), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.StdoutTruncated || result.Report.Status != ReportSucceeded {
		t.Fatalf("truncated stream lost its report: truncated=%v report=%+v", result.StdoutTruncated, result.Report)
	}
}

func TestClaudeStreamEventReadsOnlyNames(t *testing.T) {
	assistant := `{"type":"assistant","message":{"content":[{"type":"text","text":"SECRET"},` +
		`{"type":"tool_use","name":"mcp__claude-in-chrome__click","input":{"selector":"SECRET"}}]}}`
	event := claudeStreamEvent([]byte(assistant))
	if !event.Turn || event.Tool != "mcp__claude-in-chrome__click" || event.Final {
		t.Fatalf("assistant event = %+v", event)
	}
	if event := claudeStreamEvent([]byte(`{"type":"result","subtype":"success"}`)); !event.Final || event.Turn {
		t.Fatalf("result event = %+v", event)
	}
	for _, line := range []string{`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`, `not json`, `{}`} {
		if event := claudeStreamEvent([]byte(line)); event != (StreamEvent{}) {
			t.Fatalf("%q yielded %+v", line, event)
		}
	}
}

// Codex progress reports only what Codex reports: a turn is counted when Codex
// says one of its own turns completed, in either wire spelling, and never
// inferred from a message or a reasoning item. Tool names come only from events
// that name a tool, so model output is never labelled as one.
func TestCodexStreamEventReadsOnlyNames(t *testing.T) {
	cases := []struct {
		line string
		want StreamEvent
	}{
		{`{"id":"0","msg":{"type":"agent_message","message":"SECRET"}}`, StreamEvent{}},
		{`{"id":"1","msg":{"type":"agent_reasoning","text":"SECRET"}}`, StreamEvent{}},
		{`{"id":"2","msg":{"type":"task_complete","last_agent_message":"SECRET"}}`, StreamEvent{Turn: true}},
		{`{"id":"3","msg":{"type":"mcp_tool_call_begin","invocation":{"server":"chrome","tool":"navigate","arguments":{"url":"SECRET"}}}}`,
			StreamEvent{Tool: "chrome.navigate"}},
		{`{"id":"4","msg":{"type":"exec_command_begin","command":["SECRET"]}}`, StreamEvent{Tool: "exec_command"}},
		{`{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":22}}`, StreamEvent{Turn: true}},
		{`{"type":"turn.started"}`, StreamEvent{}},
		{`{"type":"turn.failed","error":{"message":"SECRET"}}`, StreamEvent{}},
		{`{"type":"item.completed","item":{"id":"item_0","item_type":"assistant_message","text":"SECRET"}}`, StreamEvent{}},
		{`{"type":"item.completed","item":{"id":"item_1","item_type":"reasoning","text":"SECRET"}}`, StreamEvent{}},
		{`{"type":"item.started","item":{"id":"item_2","item_type":"mcp_tool_call","server":"chrome","tool":"navigate","status":"in_progress"}}`,
			StreamEvent{Tool: "chrome.navigate"}},
		{`{"type":"item.completed","item":{"id":"item_3","item_type":"command_execution","command":"SECRET","aggregated_output":"SECRET","exit_code":0}}`,
			StreamEvent{Tool: "exec_command"}},
		{`{"type":"item.started","item":{"id":"item_4","item_type":"web_search","query":"SECRET"}}`, StreamEvent{Tool: "web_search"}},
		{`{"type":"item.completed","item":{"id":"item_5","item_type":"mcp_tool_call","status":"failed"}}`, StreamEvent{}},
		{`{"type":"item.completed","item":{"id":"item_6","item_type":"todo_list","items":["SECRET"]}}`, StreamEvent{}},
		{`{"type":"thread.started"}`, StreamEvent{}},
	}
	for _, c := range cases {
		got := codexStreamEvent([]byte(c.line))
		if got != c.want {
			t.Fatalf("codexStreamEvent(%s) = %+v, want %+v", c.line, got, c.want)
		}
		if got.Tool == "assistant_message" || got.Tool == "reasoning" {
			t.Fatalf("codexStreamEvent(%s) labelled model output as a tool", c.line)
		}
	}
}

// One Codex turn full of reasoning, a tool call and a message counts as one
// turn - the number the requester sees is the number Codex reported.
func TestCodexStreamCountsOnlyReportedTurns(t *testing.T) {
	recorder := &progressRecorder{}
	reader := newStreamReader(codexStreamEvent, recorder.record)
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"item_type":"reasoning","text":"SECRET"}}`,
		`{"type":"item.started","item":{"item_type":"mcp_tool_call","server":"chrome","tool":"navigate"}}`,
		`{"type":"item.completed","item":{"item_type":"assistant_message","text":"SECRET"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":11}}`,
	} {
		reader.Write([]byte(line + "\n"))
	}
	reader.Close()
	last, count := recorder.last()
	if count == 0 {
		t.Fatal("a Codex turn published no progress at all")
	}
	if last.Turns != 1 {
		t.Fatalf("one reported Codex turn was counted as %d", last.Turns)
	}
	if last.Tool != "chrome.navigate" {
		t.Fatalf("last tool = %q, want the tool the turn actually invoked", last.Tool)
	}
}

func TestStreamReaderBoundsLinesAndToolNames(t *testing.T) {
	recorder := &progressRecorder{}
	reader := newStreamReader(func(line []byte) StreamEvent {
		if len(line) > maxStreamLine {
			t.Fatal("an oversized line reached the decoder")
		}
		return StreamEvent{Turn: true, Tool: strings.TrimSpace(string(line))}
	}, recorder.record)
	// An oversized line is dropped whole, and the trailing line without a
	// newline still lands - with its name truncated to the cap.
	reader.Write([]byte(strings.Repeat("x", maxStreamLine+1) + "\n"))
	reader.Write([]byte("first-line\n"))
	reader.Write([]byte(strings.Repeat("t", maxToolNameBytes+50)))
	reader.Close()
	last, count := recorder.last()
	if count == 0 || last.Tool != strings.Repeat("t", maxToolNameBytes) {
		t.Fatalf("tool name was not bounded: %+v (%d samples)", last, count)
	}
	if last.Turns != 2 {
		t.Fatalf("turns = %d, want the oversized line skipped and the tail counted", last.Turns)
	}
}

func TestStreamReaderRetainsOnlyTheFinalLine(t *testing.T) {
	reader := newStreamReader(func(line []byte) StreamEvent {
		return StreamEvent{Final: strings.HasPrefix(string(line), "{\"type\":\"result\"")}
	}, nil)
	reader.Write([]byte("{\"type\":\"assistant\"}\n{\"type\":\"result\",\"n\":1}\n{\"type\":\"assistant\"}\n"))
	reader.Close()
	if got := string(reader.Final()); got != `{"type":"result","n":1}` {
		t.Fatalf("final line = %q", got)
	}
}

// A provider that never marks a final line hands its decoder nothing, which is
// how Codex keeps reading its report from the file it wrote.
func TestStreamWithoutFinalLineDecodesNothing(t *testing.T) {
	reader := newStreamReader(func([]byte) StreamEvent { return StreamEvent{Turn: true} }, nil)
	reader.Write([]byte("{}\n"))
	reader.Close()
	if reader.Final() != nil {
		t.Fatalf("final = %q, want none", reader.Final())
	}
	if _, err := parseClaudeReport(reader.Final()); err == nil {
		t.Fatal("a stream with no result line must not produce a report")
	}
}

// A run that never starts must leave nothing behind: the progress publisher is
// created before the process is, so every failure path has to shut it down.
func TestRunnerStopsProgressWhenTheProviderFailsToStart(t *testing.T) {
	unrunnable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(unrunnable, []byte("#!/nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &progressRecorder{}
	runner := newStreamRunner(t, &unstartableProvider{executable: unrunnable}, 1024)
	baseline := runtime.NumGoroutine()
	if _, err := runner.Run(context.Background(), helperTask(23), recorder.record); err == nil {
		t.Fatal("an unrunnable executable started")
	}
	for i := 0; runtime.NumGoroutine() > baseline && i < 200; i++ {
		time.Sleep(time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 0 {
		t.Fatalf("%d goroutines outlived the failed start", leaked)
	}
	if _, count := recorder.last(); count != 0 {
		t.Fatalf("a run that never started published %d samples", count)
	}
}

type unstartableProvider struct{ executable string }

func (p *unstartableProvider) Name() string { return "fake" }

func (p *unstartableProvider) Prepare(Task, string) (Invocation, error) {
	dir, err := os.Getwd()
	if err != nil {
		return Invocation{}, err
	}
	return Invocation{Executable: p.executable, Dir: dir, Prompt: []byte("brief"),
		Stream: claudeStreamEvent, Decode: parseClaudeReport}, nil
}

// nearLimitReport is a valid report whose Claude envelope lands above 1 MiB but
// within what the report parser accepts - the band a stream run used to drop.
func nearLimitReport() Report {
	item := strings.Repeat("o", maxItemBytes-1)
	report := Report{Status: ReportSucceeded, Summary: strings.Repeat("s", maxSummaryBytes-1),
		Observations: make([]string, 0, maxReportItems), Actions: make([]string, 0, maxReportItems), Evidence: []string{}}
	for i := 0; i < maxReportItems; i++ {
		report.Observations = append(report.Observations, item)
		report.Actions = append(report.Actions, item)
	}
	return report
}

// claudeResultEnvelope wraps a report the way Claude's result event does,
// accounting fields included, so the fixture carries real envelope overhead.
func claudeResultEnvelope(report Report) (string, error) {
	data, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"duration_ms":123456,`+
		`"num_turns":%d,"session_id":"00000000-0000-4000-8000-000000000000","total_cost_usd":0.0123,`+
		`"structured_output":%s}`, streamHelperTurns, data), nil
}

func helperReport(t *testing.T, provider *streamProvider, id uint64) Report {
	t.Helper()
	result, err := newStreamRunner(t, provider, defaultOutput).Run(context.Background(), helperTask(id), nil)
	if err != nil {
		t.Fatalf("%s run: %v", provider.mode, err)
	}
	return result.Report
}

// The two output modes must accept the same reports: stream assembly is bounded
// by the report parser's own limit, so a near-limit report that the single-blob
// path parses can never be dropped as an oversized stream line.
func TestRunnerParsesNearLimitReportInBothModes(t *testing.T) {
	envelope, err := claudeResultEnvelope(nearLimitReport())
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope) <= 1<<20 || len(envelope) > defaultOutput {
		t.Fatalf("fixture is %d bytes; it must exceed 1 MiB and stay within %d to cover the boundary", len(envelope), defaultOutput)
	}
	blob := helperReport(t, &streamProvider{mode: "blob-limit"}, 24)
	streamed := helperReport(t, &streamProvider{mode: "stream-limit", stream: true}, 25)
	if blob.Status != ReportSucceeded {
		t.Fatalf("blob mode lost the near-limit report: %+v", blob)
	}
	if !reflect.DeepEqual(blob, streamed) {
		t.Fatal("stream mode did not parse the near-limit report identically to blob mode")
	}
}
