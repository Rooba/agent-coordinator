#!/usr/bin/env bash
# Records two Claude Code and two Codex sessions coordinating through
# agent-coordinator, then renders the cast to GIF and MP4.
#
# Usage: record.sh [all|setup|launch|ready|start|label|go|wait|stop|render|clean]
# "all" runs every stage in order. Stages can be run one at a time to debug.
#
# Needs tmux, asciinema 3, agg, gifsicle, ffmpeg, jq, rg, python3 and go, plus
# logged-in `claude` and `codex` CLIs and an installed agent-coordinator.
# It runs four live agent sessions, so it spends real model quota.
set -euo pipefail

# ---- knobs ---------------------------------------------------------------
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out}"                        # where demo.cast/gif/mp4 land
CFG="${CFG:-/tmp/tidepool-cfg}"                # demo-only Claude settings, MCP config, Go cache
# Each run gets a fresh repo, so a fresh coordinator scope: empty board, no edit
# history. Only the parent changes, so the TUIs keep showing ".../tidepool".
RUN_ID="${RUN_ID:-}"                           # empty: setup picks the next free /tmp/rN
WS="${WS:-${RUN_ID:+/tmp/$RUN_ID/tidepool}}"
WS="${WS:-$(cat "$CFG/ws" 2>/dev/null || true)}"   # later stages reuse setup's choice
COLS="${COLS:-150}"                            # recorded terminal size
ROWS="${ROWS:-44}"
STRIP="${STRIP:-6}"                            # height of the board strip on top
CLAUDE_MODEL="${CLAUDE_MODEL:-sonnet}"
CODEX_EFFORT="${CODEX_EFFORT:-low}"
RUN_TIMEOUT="${RUN_TIMEOUT:-420}"              # give up on a run after this many seconds
INTRO="${INTRO:-50}"                           # first N recorded seconds (the hand-out) play slower
INTRO_SPEED="${INTRO_SPEED:-2}"
SPEED="${SPEED:-5}"                            # speed-up for the rest of the run
IDLE="${IDLE:-1}"                              # cap idle gaps in the GIF (seconds)
FONT_SIZE="${FONT_SIZE:-14}"
FONT="${FONT:-DejaVu Sans Mono,Noto Color Emoji}"
THEME="${THEME:-github-dark}"
SOCK=tidepool                                  # tmux socket name for the grid
# Machine-specific extras, empty by default:
CODEX_EXTRA_ARGS="${CODEX_EXTRA_ARGS:-}"       # more Codex flags, e.g. "-c mcp_servers.X.enabled=false"
EXTRA_ENV="${EXTRA_ENV:-}"                     # more VAR=value pairs for the agents' environment
GOBIN_DIR="${GOBIN_DIR:-}"                     # Go toolchain bin dir; default: wherever `go` resolves
PROMPT_FILE="${PROMPT_FILE:-$HERE/prompt.txt}"

AC="$(command -v agent-coordinator)"
GOBIN_DIR="${GOBIN_DIR:-$(dirname "$(command -v go)")}"
TM=(tmux -L "$SOCK")
mkdir -p "$OUT"

# ---- helpers -------------------------------------------------------------

# Clean environment for the nested sessions: no parent Claude Code variables,
# but the same XDG_RUNTIME_DIR so every agent reaches the same daemon socket.
# $CFG/bin and the demo git identity keep the local user name off screen.
clean_env() {
  env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" SHELL=/bin/bash \
    TERM=xterm-256color COLORTERM=truecolor LANG=C.UTF-8 \
    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" \
    PATH="$CFG/bin:$GOBIN_DIR:$(dirname "$AC"):/usr/local/bin:/usr/bin:/bin" \
    GOCACHE="$CFG/gocache" GOTOOLCHAIN=local GOFLAGS=-mod=mod \
    GIT_CONFIG_GLOBAL="$CFG/gitconfig" GIT_CONFIG_NOSYSTEM=1 \
    $EXTRA_ENV "$@"
}

pane_text() { "${TM[@]}" capture-pane -p -t "$1"; }

# Block until a pane shows a regex, or fail after N seconds.
wait_for() {
  local pane=$1 re=$2 secs=${3:-60}
  for ((i = 0; i < secs * 2; i++)); do
    rg -q -- "$re" <<<"$(pane_text "$pane")" && return 0
    sleep 0.5
  done
  echo "timeout waiting for /$re/ in $pane" >&2
  return 1
}

# ---- stages --------------------------------------------------------------

# A tiny Go package with four independent stub functions and their tests.
setup() {
  if [ -z "$RUN_ID" ]; then
    local n=1
    while [ -e "/tmp/r$n" ]; do n=$((n + 1)); done
    WS="/tmp/r$n/tidepool"
  fi
  rm -rf "$WS" "$CFG"
  mkdir -p "$WS" "$CFG/gocache" "$CFG/bin"
  echo "$WS" >"$CFG/ws"
  printf '[user]\n\tname = demo\n\temail = demo@example.invalid\n' >"$CFG/gitconfig"
  # Long listings show numeric owners instead of the local account name.
  printf '#!/bin/sh\nexec %s -n "$@"\n' "$(command -v ls)" >"$CFG/bin/ls"
  chmod +x "$CFG/bin/ls"
  cd "$WS"
  cat >go.mod <<'EOF'
module tidepool

go 1.22
EOF
  cat >TODO.md <<'EOF'
# TODO

Each task is one stub in its own file. Implement it so its test passes.

- [ ] reverse.go    Reverse(s) returns s with its runes in reverse order
- [ ] slug.go       Slug(s) lowercases s and joins its words with "-"
- [ ] vowels.go     Vowels(s) counts a, e, i, o, u (any case)
- [ ] palindrome.go Palindrome(s) reports whether s reads the same reversed, ignoring case and spaces
EOF
  stub() { # file func signature test-body
    printf 'package tidepool\n\n%s {\n\tpanic("TODO")\n}\n' "$3" >"$1.go"
    printf 'package tidepool\n\nimport "testing"\n\nfunc Test%s(t *testing.T) {\n%s\n}\n' "$2" "$4" >"$1_test.go"
  }
  stub reverse Reverse 'func Reverse(s string) string' \
    $'\tif got := Reverse("tide pool"); got != "loop edit" {\n\t\tt.Fatalf("got %q", got)\n\t}'
  stub slug Slug 'func Slug(s string) string' \
    $'\tif got := Slug("  Tide Pool  Crab "); got != "tide-pool-crab" {\n\t\tt.Fatalf("got %q", got)\n\t}'
  stub vowels Vowels 'func Vowels(s string) int' \
    $'\tif got := Vowels("Anemone Urchin"); got != 6 {\n\t\tt.Fatalf("got %d", got)\n\t}'
  stub palindrome Palindrome 'func Palindrome(s string) bool' \
    $'\tif !Palindrome("Never odd or even") || Palindrome("tide pool") {\n\t\tt.Fatal("wrong")\n\t}'
  git init -q && git add -A && git -c user.name=demo -c user.email=demo@example.invalid commit -qm init

  # Claude Code: only this demo's settings, MCP server, and coordinator hooks.
  cat >"$CFG/claude-mcp.json" <<EOF
{"mcpServers": {"agent-coordinator": {"command": "$AC", "args": ["mcp"]}}}
EOF
  local hook='{"hooks":[{"type":"command","command":"'"$AC"' hook","timeout":10}]}'
  local events=(SessionStart UserPromptSubmit PreToolUse PostToolUse Stop SessionEnd)
  local hooks="" e
  for e in "${events[@]}"; do hooks+="\"$e\":[$hook],"; done
  cat >"$CFG/claude-settings.json" <<EOF
{
  "hooks": {${hooks%,}},
  "permissions": {"allow": [
    "mcp__agent-coordinator", "Read", "Edit", "Write",
    "Bash(go test:*)", "Bash(go build:*)", "Bash(go vet:*)", "Bash(gofmt:*)",
    "Bash(agent-coordinator:*)"
  ]},
  "claudeMdExcludes": ["$HOME/**"],
  "spinnerTipsEnabled": false,
  "promptSuggestionEnabled": false
}
EOF
}

launch() {
  "${TM[@]}" kill-server 2>/dev/null || true
  check_board_empty
  local claude_cmd="claude --model $CLAUDE_MODEL --setting-sources project --strict-mcp-config \
--mcp-config $CFG/claude-mcp.json --settings $CFG/claude-settings.json --disable-slash-commands"
  # Trusting the folder through an in-memory override keeps Codex from saving it to config.toml.
  # Codex: auto-approve the coordinator tools and let the sandbox reach its unix socket.
  local codex_cmd="codex -s workspace-write -a never -c model_reasoning_effort=$CODEX_EFFORT \
-c check_for_update_on_startup=false -c 'projects={\"$WS\"={trust_level=\"trusted\"}}' \
-c mcp_servers.agent-coordinator.default_tools_approval_mode=\"approve\" \
-c sandbox_workspace_write.network_access=true \
-c 'sandbox_workspace_write.writable_roots=[\"$CFG\",\"$HOME/.local/state/agent-coordinator/wait\"]' $CODEX_EXTRA_ARGS"

  cd "$WS"
  clean_env "${TM[@]}" -f /dev/null new-session -d -s demo -x "$COLS" -y "$ROWS" -c "$WS" "exec $HERE/board.sh"
  local opt
  for opt in "status off" "pane-border-status top" "pane-border-format ' #[bold]#{pane_title} '" \
    "pane-border-lines heavy" "pane-border-style fg=colour240" "pane-active-border-style fg=colour240" \
    "allow-set-title off" "window-size manual" "focus-events on"; do
    eval "${TM[*]} set -g $opt"
  done
  # Board strip on top, Claude Code on the top row, Codex on the bottom row.
  local c1 c2
  c1=$("${TM[@]}" split-window -v -P -F '#{pane_id}' -t demo:0.0 -c "$WS" "$claude_cmd")
  c2=$("${TM[@]}" split-window -h -P -F '#{pane_id}' -t "$c1" -c "$WS" "$claude_cmd")
  "${TM[@]}" split-window -v -t "$c1" -c "$WS" "$codex_cmd" \; select-pane -T "Codex 1"
  "${TM[@]}" split-window -v -t "$c2" -c "$WS" "$codex_cmd" \; select-pane -T "Codex 2"
  "${TM[@]}" select-pane -t demo:0.0 -T "agent-coordinator board"
  "${TM[@]}" select-pane -t "$c1" -T "Claude Code 1"
  "${TM[@]}" select-pane -t "$c2" -T "Claude Code 2"
  "${TM[@]}" resize-pane -t demo:0.0 -y "$STRIP"
}

# Pane ids in reading order: board, claude1, claude2, codex1, codex2.
panes() { "${TM[@]}" list-panes -t demo -F '#{pane_top} #{pane_left} #{pane_id}' | sort -n -k1,1 -k2,2 | cut -d' ' -f3; }

# Answer Claude's one-time folder trust dialog and wait for every input box.
ready() {
  local p
  for p in $(panes | sed 1d); do
    wait_for "$p" "Yes, I trust this folder|shift\+tab to cycle|Ask Codex" 60
    if rg -q "Yes, I trust this folder" <<<"$(pane_text "$p")"; then
      "${TM[@]}" send-keys -t "$p" Down Enter
      wait_for "$p" "shift\+tab to cycle" 30
    fi
  done
  sleep 2
}

# Rename each pane to "<harness> - <coordinator name>" once the agent's name shows up.
label() {
  local -A done_=()
  while "${TM[@]}" has-session -t demo 2>/dev/null; do
    local p title name
    for p in $(panes | sed 1d); do
      [ -n "${done_[$p]:-}" ] && continue
      # An agent's own name shows in "I am <name>" or in its own wait command.
      name=$("${TM[@]}" capture-pane -p -S -500 -t "$p" |
        rg -o --replace '$1$2' "I am \`?([a-z]+-[a-z]+)|wait '([a-z]+-[a-z]+)'" | head -1 || true)
      [ -z "$name" ] && continue
      title=$("${TM[@]}" display -p -t "$p" '#{pane_title}')
      "${TM[@]}" select-pane -t "$p" -T "$title - $name"
      done_[$p]=1
    done
    sleep 1
  done
}

start() {
  rm -f "$OUT/demo.cast"
  # Record the composite grid by attaching a second client in a headless pty.
  clean_env asciinema rec --headless --overwrite -f asciicast-v2 --window-size "${COLS}x${ROWS}" \
    -c "tmux -L $SOCK attach -t demo" "$OUT/demo.cast" >/dev/null 2>&1 &
  echo $! >"$CFG/asciinema.pid"
  sleep 1
}

go_prompt() {
  "${TM[@]}" load-buffer -b prompt "$PROMPT_FILE"
  for p in $(panes | sed 1d); do
    "${TM[@]}" paste-buffer -p -b prompt -t "$p"
  done
  sleep 1
  for p in $(panes | sed 1d); do "${TM[@]}" send-keys -t "$p" Enter; done
}

# Wait until the whole package passes and every agent has gone quiet.
wait_done() {
  local s
  for ((s = 0; s < RUN_TIMEOUT; s += 5)); do
    (cd "$WS" && clean_env go test ./... >/dev/null 2>&1) && break
    sleep 5
  done
  ((s >= RUN_TIMEOUT)) && echo "run did not finish in ${RUN_TIMEOUT}s" >&2
  for ((s = 0; s < 90; s += 3)); do
    local busy=0 p
    for p in $(panes | sed 1d); do rg -q "esc to interrupt" <<<"$(pane_text "$p")" && busy=1; done
    ((busy)) || break
    sleep 3
  done
  sleep 4 # hold the final frame
}

# End the recording, then exit each agent cleanly so the board marks it gone.
stop() {
  "${TM[@]}" list-clients -F '#{client_name}' 2>/dev/null | xargs -r -n1 "${TM[@]}" detach-client -t 2>/dev/null || true
  local pid p
  pid=$(cat "$CFG/asciinema.pid" 2>/dev/null || true)
  sleep 1
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  for p in $(panes 2>/dev/null | sed 1d); do
    # Panes close as soon as their agent exits, so later keys may have no target.
    "${TM[@]}" send-keys -t "$p" Escape C-c 2>/dev/null || true; sleep 0.3
    "${TM[@]}" send-keys -t "$p" C-c 2>/dev/null || true; sleep 0.3
    "${TM[@]}" send-keys -t "$p" C-d 2>/dev/null || true
  done
  # Give each agent time to run its exit hook before the server goes away.
  for ((i = 0; i < 20; i++)); do
    [ "$("${TM[@]}" list-panes -t demo 2>/dev/null | wc -l)" -le 1 ] && break
    sleep 1
  done
  "${TM[@]}" kill-server 2>/dev/null || true
}

# Refuse to start while agents from an earlier run still look alive on the board.
check_board_empty() {
  local live
  live=$(cd "$WS" && "$AC" board --live 2>/dev/null | rg -v "^no agents" || true)
  [ -z "$live" ] && return 0
  echo "agents from an earlier run are still live in $WS (they turn stale within 15 min):" >&2
  echo "$live" >&2
  return 1
}

render() {
  cd "$OUT"
  # Mask the local account, cut the tail where the client detaches, then retime:
  # slow intro, fast middle, capped gaps.
  python3 - demo.cast demo.timed.cast "$INTRO" "$INTRO_SPEED" "$SPEED" "$IDLE" "$HOME" "$USER" <<'PY'
import json, sys
src, dst = sys.argv[1], sys.argv[2]
intro, slow, fast, idle = map(float, sys.argv[3:7])
home, user = sys.argv[7], sys.argv[8]
# Same-length stand-ins keep every column where the terminal drew it.
alias = ("demo" * len(user))[:len(user)]
def mask(line):
    line = line.replace(home, home.replace(user, alias))
    return line.replace(f" {user} {user} ", f" {alias} {alias} ")
lines = [mask(l) for l in open(src).read().splitlines()]
events = [json.loads(l) for l in lines[1:]]
end = next((i for i, e in enumerate(events) if "[detached" in e[2]), len(events))
if end and "\x1b[2J" in events[end - 1][2]:
    end -= 1
events = events[:end]
open(src, "w").write("\n".join([lines[0]] + lines[1:end + 1]) + "\n")
out, prev_t, prev_o = [], 0.0, 0.0
for t, kind, data in events:
    dt = t - prev_t
    rate = slow if t <= intro else fast
    prev_o += min(dt / rate, idle)
    prev_t = t
    out.append(json.dumps([round(prev_o, 4), kind, data]))
open(dst, "w").write("\n".join([lines[0]] + out) + "\n")
PY
  agg --speed 1 --idle-time-limit "$IDLE" --fps-cap 8 --font-size "$FONT_SIZE" \
    --font-family "$FONT" --theme "$THEME" --last-frame-duration 4 \
    demo.timed.cast demo.raw.gif >/dev/null 2>&1
  rm -f demo.timed.cast
  gifsicle -O3 --lossy=40 --colors 128 demo.raw.gif -o demo.gif
  # H.264 for broad playback; openSUSE's ffmpeg ships OpenH264 instead of x264.
  local enc=libx264 q=(-crf 22)
  rg -q libx264 <<<"$(ffmpeg -hide_banner -encoders 2>/dev/null)" || { enc=libopenh264; q=(-b:v 1500k); }
  ffmpeg -y -loglevel error -i demo.raw.gif -movflags +faststart -pix_fmt yuv420p -r 8 \
    -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" -c:v "$enc" "${q[@]}" demo.mp4
  rm -f demo.raw.gif
  # A handful of stills spread across the result, ending on the final frame.
  rm -rf frames && mkdir frames
  local dur n=5 i t
  dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 demo.mp4)
  for i in $(seq 1 $n); do
    t=$(awk -v d="$dur" -v i="$i" -v n="$n" 'BEGIN { printf "%.2f", (i == n) ? d - 0.3 : d * (i - 0.5) / n }')
    ffmpeg -y -loglevel error -ss "$t" -i demo.mp4 -frames:v 1 "frames/frame-$i.png"
  done
}

clean() {
  stop
  rm -rf /tmp/r[0-9]*/tidepool "$CFG"
  rmdir /tmp/r[0-9]* 2>/dev/null || true
}

case "${1:-all}" in
  all) setup; launch; ready; start; label & go_prompt; wait_done; stop; render ;;
  setup) setup ;; launch) launch ;; ready) ready ;; start) start ;;
  label) label ;; go) go_prompt ;; wait) wait_done ;; stop) stop ;; render) render ;; clean) clean ;;
  *) echo "unknown stage: $1" >&2; exit 2 ;;
esac
