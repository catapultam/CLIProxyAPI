#!/bin/sh
# agentbus setup: installs the agentbus wake hook for Claude Code on this
# machine. Served by the proxy at /v1/agentbus/setup. Safe to run again.
set -e

if [ -z "$ANTHROPIC_BASE_URL" ] || [ -z "$ANTHROPIC_AUTH_TOKEN" ]; then
	echo "agentbus setup: ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN must be set (they are in Claude Code sessions that use the proxy)" >&2
	exit 1
fi
PY=$(command -v python3 || command -v python || true)
if [ -z "$PY" ]; then
	echo "agentbus setup: python3 is required" >&2
	exit 1
fi

# Resolve the Claude Code config dir and the hook path the way Claude Code
# sees them (Windows paths with forward slashes on Windows).
PATHS=$("$PY" -c '
import os
base = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.join(os.path.expanduser("~"), ".claude")
base = os.path.abspath(base)
print(base.replace("\\", "/"))
print(os.path.join(base, "hooks", "agentbus", "wait.sh").replace("\\", "/"))
')
CONFIG_DIR=$(printf '%s\n' "$PATHS" | sed -n 1p)
WAITER=$(printf '%s\n' "$PATHS" | sed -n 2p)

mkdir -p "$(dirname "$WAITER")"
curl -fsS -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/agentbus/wait.sh" -o "$WAITER.tmp"
mv "$WAITER.tmp" "$WAITER"
chmod +x "$WAITER"

"$PY" - "$CONFIG_DIR/settings.json" "$WAITER" <<'PYEOF'
import json, os, shutil, sys

path, command = sys.argv[1], sys.argv[2]
data = {}
if os.path.exists(path):
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    backup = path + ".bak-agentbus"
    if not os.path.exists(backup):
        shutil.copyfile(path, backup)
hooks = data.setdefault("hooks", {})
changed = False
for event in ("SessionStart", "Stop"):
    groups = hooks.setdefault(event, [])
    present = any(h.get("command") == command for g in groups for h in g.get("hooks", []))
    if present:
        continue
    groups.append({"matcher": "", "hooks": [{"type": "command", "command": command, "asyncRewake": True}]})
    changed = True
if changed:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8", newline="\n") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    os.replace(tmp, path)
    print("agentbus: wake hook added to " + path)
else:
    print("agentbus: wake hook already present in " + path)
PYEOF

echo "agentbus: setup complete. New sessions on this machine can be woken by messages; restart this session to be woken too."
