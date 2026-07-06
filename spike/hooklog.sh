#!/bin/sh
# spike/hooklog.sh - append each hook invocation's stdin JSON as one line.
LOG="${AC_SPIKE_LOG:-/tmp/ac-spike-hooklog.jsonl}"
payload="$(cat)"
printf '%s\n' "$payload" >> "$LOG"
# Inject a marker via PostToolUse additionalContext to verify injection works.
case "$payload" in
  *'"hook_event_name":"PostToolUse"'*|*'"hook_event_name": "PostToolUse"'*)
    printf '%s' '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"[spike-marker] injection works"}}'
    ;;
esac
exit 0
