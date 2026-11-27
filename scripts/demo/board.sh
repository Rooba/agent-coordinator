#!/usr/bin/env bash
# Live view of the coordinator board for the demo workspace, redrawn in place.
tput civis 2>/dev/null
clear
while :; do
  w=$(tput cols)
  {
    printf '%-18s %-7s %-18s %s\n' AGENT STATUS CLAIMS ACTIVITY
    agent-coordinator board --json 2>/dev/null | jq -r '
      (. // []) | sort_by(.name)[] | [.name, .status,
        ((.claims // []) | map(split("/") | last) | join(",") | if . == "" then "-" else . end),
        ((.current_task // .activity // "") | gsub("\\s+"; " "))] | @tsv' |
      while IFS=$'\t' read -r name status claims act; do
        printf '%-18s %-7s %-18s %s\n' "$name" "$status" "$claims" "$act"
      done
  } | cut -c1-"$w" | {
    tput cup 0 0
    while IFS= read -r line; do printf '%s\033[K\n' "$line"; done
    printf '\033[J'
  }
  sleep 1
done
