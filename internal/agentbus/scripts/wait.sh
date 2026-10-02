#!/bin/sh
# agentbus waiter: a Claude Code SessionStart and Stop hook with asyncRewake.
# Long-polls the proxy for messages to this session. When one arrives it
# prints it to stderr and exits 2, which wakes the session. A newer waiter for
# the same session (409) or missing configuration exits 0 silently.

# find_python sets PY (and PYARGS for the Windows py launcher) to the first
# interpreter that actually runs; the Windows Store python3 stub does not.
find_python() {
	if [ -n "$AGENTBUS_PYTHON" ] && "$AGENTBUS_PYTHON" -c 'import json, socket, urllib.parse' >/dev/null 2>&1; then
		PY=$AGENTBUS_PYTHON PYARGS=
		return 0
	fi
	for cand in python3 python; do
		if "$cand" -c 'import json, socket, urllib.parse' >/dev/null 2>&1; then
			PY=$cand PYARGS=
			return 0
		fi
	done
	if py -3 -c 'import json, socket, urllib.parse' >/dev/null 2>&1; then
		PY=py PYARGS=-3
		return 0
	fi
	return 1
}
NO_PYTHON_MSG="no working Python found (tried \$AGENTBUS_PYTHON, python3, python, py -3). Install Python 3, or set AGENTBUS_PYTHON to a working interpreter in the env block of Claude Code settings, then run setup again."

# --check: verify this machine can run the waiter, for setup and for agents
# diagnosing a session that is never woken.
if [ "$1" = "--check" ]; then
	if ! find_python; then
		echo "agentbus check: $NO_PYTHON_MSG" >&2
		exit 1
	fi
	if [ -z "$ANTHROPIC_BASE_URL" ] || [ -z "$ANTHROPIC_AUTH_TOKEN" ]; then
		echo "agentbus check: ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN must be set (they come from the env block of Claude Code settings)." >&2
		exit 1
	fi
	CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 		-H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/agentbus/peers") || CODE=000
	case "$CODE" in
	200)
		echo "agentbus: OK (python: $PY $PYARGS, proxy: $ANTHROPIC_BASE_URL)"
		exit 0
		;;
	401 | 403)
		echo "agentbus check: the proxy rejected ANTHROPIC_AUTH_TOKEN (HTTP $CODE)." >&2
		;;
	000)
		echo "agentbus check: cannot reach $ANTHROPIC_BASE_URL (is this machine on the tailnet?)." >&2
		;;
	*)
		echo "agentbus check: unexpected HTTP $CODE from $ANTHROPIC_BASE_URL/v1/agentbus/peers." >&2
		;;
	esac
	exit 1
fi

[ -n "$ANTHROPIC_BASE_URL" ] && [ -n "$ANTHROPIC_AUTH_TOKEN" ] || exit 0
find_python || exit 0

INPUT=$(cat)
# Line 1: query string for /wait. Line 2: session id. The friendly name is the
# latest /rename title recorded in the transcript.
PARSED=$(printf '%s' "$INPUT" | "$PY" $PYARGS -c '
import json, socket, sys, urllib.parse
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
name = ""
tp = d.get("transcript_path") or ""
if tp:
    try:
        with open(tp, encoding="utf-8", errors="replace") as f:
            for line in f:
                if "\"custom-title\"" in line:
                    try:
                        name = json.loads(line).get("customTitle") or name
                    except Exception:
                        pass
    except Exception:
        pass
sid = d.get("session_id") or ""
if not sid:
    sys.exit(1)
print(urllib.parse.urlencode({"session": sid, "machine": socket.gethostname(), "cwd": d.get("cwd") or "", "name": name}))
print(sid)
') || exit 0
QUERY=$(printf '%s\n' "$PARSED" | sed -n 1p)
SESSION=$(printf '%s\n' "$PARSED" | sed -n 2p)

TMP="${TMPDIR:-/tmp}/agentbus-$$.json"
trap 'rm -f "$TMP"' EXIT
START=$(date +%s)
FAILS=0
while :; do
	# Give up after a day so waiters of closed sessions do not poll forever.
	[ $(( $(date +%s) - START )) -lt 86400 ] || exit 0
	CODE=$(curl -s -o "$TMP" -w '%{http_code}' --max-time 70 \
		-H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" \
		"$ANTHROPIC_BASE_URL/v1/agentbus/wait?$QUERY") || CODE=000
	case "$CODE" in
	200)
		"$PY" $PYARGS - "$TMP" "$SESSION" >&2 <<'PYEOF'
import json, sys
path, session = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as f:
    msgs = json.load(f).get("messages", [])
for m in msgs:
    head = "agentbus message %s from %s" % (m.get("id"), m.get("from"))
    if m.get("reply_to"):
        head += " (in reply to %s)" % m["reply_to"]
    print(head + ":")
    print(m.get("body", ""))
    print()
print("Reply with: curl -s -H \"Authorization: Bearer $ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_BASE_URL/v1/agentbus/send\" "
      "-d '{\"from_session\":\"%s\",\"to\":\"<sender address or name>\",\"reply_to\":\"<message id>\",\"body\":\"...\"}'" % session)
PYEOF
		exit 2
		;;
	204)
		FAILS=0
		;;
	409)
		exit 0
		;;
	*)
		FAILS=$((FAILS + 1))
		[ "$FAILS" -le 6 ] || exit 0
		sleep $((FAILS * 10))
		;;
	esac
done
