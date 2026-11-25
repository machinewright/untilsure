#!/bin/sh
# Test helper: fails (exit 1) with probability $1, e.g. 0.05. Optional $2: sleep this long first.
[ -n "$2" ] && sleep "$2"
seed=$(od -An -N4 -tu4 /dev/urandom)
awk -v p="$1" -v s="$seed" 'BEGIN { srand(s); exit (rand() < p) ? 1 : 0 }'
