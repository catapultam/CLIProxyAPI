#!/bin/sh
# agentbus waiter: a Claude Code SessionStart and Stop hook with asyncRewake.
# Long-polls the proxy for messages to this session. When one arrives it
# prints it to stderr and exits 2, which wakes the session. A newer waiter for
# the same session (409) or missing configuration exits 0 silently.

[ -n "$ANTHROPIC_BASE_URL" ] && [ -n "$ANTHROPIC_AUTH_TOKEN" ] || exit 0
PY=$(command -v python3 || command -v python || true)
[ -n "$PY" ] || exit 0

INPUT=$(cat)
# Line 1: query string for /wait. Line 2: session id. The friendly name is the
# latest /rename title recorded in the transcript.
PARSED=$(printf '%s' "$INPUT" | "$PY" -c '
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
		"$PY" - "$TMP" "$SESSION" >&2 <<'PYEOF'
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
