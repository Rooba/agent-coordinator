#!/bin/sh
# e2e-messaging.sh - live end-to-end test: two headless claude sessions in the
# same throwaway repo exchange a DM through the agent coordinator.
#
#   Session A "works" (runs sleep via Bash) until a [coordinator] notice about
#   new messages is pushed into its context via the PostToolUse hook, then
#   calls read_messages and prints the body.
#   Session B calls list_agents, finds A, and send_message's it 'e2e-ping-42'.
#
# Requires: agent-coordinator installed for real (socket unit + user hooks +
# user-scope MCP server) and the claude CLI on PATH.
#
# Usage: scripts/e2e-messaging.sh [repo-dir]
# A unique repo dir per run (the default) gives the test a fresh scope, so
# leftover registrations from earlier runs cannot pollute list_agents.
set -eu

command -v claude >/dev/null || { echo "E2E FAIL: claude CLI not on PATH"; exit 1; }
command -v git >/dev/null || { echo "E2E FAIL: git not on PATH"; exit 1; }

CLEANUP_REPO=""
if [ "$#" -ge 1 ]; then
    REPO="$1"
else
    REPO="/tmp/ac-e2e-$$-$(date +%s)"
    CLEANUP_REPO="$REPO"
fi
mkdir -p "$REPO"
git -C "$REPO" init -q 2>/dev/null || true

MCP=mcp__agent-coordinator
ALLOWED="Bash(sleep:*),${MCP}__status_board,${MCP}__list_agents,${MCP}__send_message,${MCP}__read_messages,${MCP}__broadcast"

LOG_A=$(mktemp) LOG_B=$(mktemp)

PROMPT_A="You are agent A in a coordinator e2e test. A [coordinator] note at \
session start gave you your agent name in this workspace - remember it. Run \
the Bash command 'sleep 5' up to eight separate times, one Bash tool call at \
a time. After each call, check whether a [coordinator] notice about new \
messages has appeared in your context. As soon as one appears, stop sleeping \
and call the read_messages tool (MCP server agent-coordinator) with 'from' \
set to your agent name. Your final reply must contain exactly one line of \
the form 'E2E-GOT: <body>' where <body> is the verbatim body of the message \
you received, or 'E2E-GOT: nothing' if no notice appeared after all eight \
sleeps."

PROMPT_B="You are agent B in a coordinator e2e test. A [coordinator] note at \
session start gave you your agent name in this workspace. Call the \
list_agents tool (MCP server agent-coordinator). Exactly one OTHER agent is \
present besides you - send it a direct message with body 'e2e-ping-42' using \
the send_message tool ('from' = your agent name, 'to' = the other agent's \
name). Your final reply must contain exactly one line: 'E2E-SENT' if the \
send succeeded, or 'E2E-FAIL: <reason>' otherwise."

# Sessions resolve their coordinator scope from their cwd, so both must be
# launched FROM the e2e repo (subshell cd, not --add-dir). Each session is
# bounded by timeout(1) so a regressed push path fails the gate fast
# instead of hanging it.
( cd "$REPO" && timeout 300 claude -p "$PROMPT_A" --allowedTools "$ALLOWED" ) > "$LOG_A" 2>&1 &
PID_A=$!
sleep 8
( cd "$REPO" && timeout 300 claude -p "$PROMPT_B" --allowedTools "$ALLOWED" ) > "$LOG_B" 2>&1 || true
wait "$PID_A" || true

echo "--- session A output ---"
cat "$LOG_A"
echo "--- session B output ---"
cat "$LOG_B"
echo "------------------------"

# On failure the temp logs and repo are intentionally left behind for postmortem.
FAIL=0
grep -q "E2E-SENT" "$LOG_B" || { echo "E2E FAIL: B did not report E2E-SENT"; FAIL=1; }
grep -q "E2E-GOT: .*e2e-ping-42" "$LOG_A" || { echo "E2E FAIL: A did not receive the DM"; FAIL=1; }
[ "$FAIL" -eq 0 ] || exit 1

rm -f "$LOG_A" "$LOG_B"
[ -n "$CLEANUP_REPO" ] && rm -rf "$CLEANUP_REPO"
echo "E2E PASS"
