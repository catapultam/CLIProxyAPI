# Agent command registry

Owners (the users seeded from `slack.allowed-emails`) run commands on an agent
from Slack with `!name args`: in the agent's thread or DM, or at the top level
as `agent: !name args`. Allowed non-owners are refused. `!commands` lists the
registry.

A name that isn't in the registry runs the Claude Code slash command of that
name (`!compact` runs `/compact`). A registry command shadows a slash command
of the same name.

## Where the files live

One `<name>.yaml` per command in `agent-commands/` in the proxy's state
directory: `WRITABLE_PATH` when it is set, otherwise the directory of
`config.yaml` (next to `slack-state.json`). A missing directory means no
registry commands.

The proxy re-reads the directory when a file is added, removed or changed, so
a new command needs no restart and no plugin redeploy.

- The name is the file name: `^[a-z0-9][a-z0-9_-]{0,31}$`. `commands`,
  `channel` and `dm` are built in and can't be used.
- Files are parsed strictly. An unknown key, a second YAML document, an empty
  file or a file over 256 KiB disables the command.
- A disabled command answers "`!name` is misconfigured" and is logged at Warn
  once per change. It never falls through to the slash command of the same
  name.

A file here defines programs that run on agents' machines, so only root may
change the registry. The directory and its files must be owned by root and
not group- or world-writable:

```sh
install -d -m 755 -o root -g root <state dir>/agent-commands
install -m 644 -o root -g root screenshot.yaml <state dir>/agent-commands/
```

`screenshot.yaml` in this directory is a ready-made `!screenshot`. Install it
with the second command.

## Fields

| Field | Kinds | Meaning |
| --- | --- | --- |
| `description` | all | Shown by `!commands`. |
| `kind` | all | `slash`, `prompt` or `shell`. |
| `command` | slash | The slash command, without `/` (`compact`, `plugin:cmd`). |
| `args` | slash | Optional template for its arguments. `{args}` is the text after `!name`. Without `{args}` the command takes no arguments. With no template, the arguments pass through as they are. |
| `text` | prompt | Submitted to the agent as if an owner had typed it. `{args}` is replaced with the arguments. |
| `argv` | shell | Per OS (`windows`, `darwin`, `linux`): the program and its arguments. It runs without a shell. |
| `env` | shell | Extra environment variables (name to value). |
| `output` | shell | `text` (the default: stdout, or stderr on failure) or `image` (the file at `{out}` is posted). |
| `timeout_seconds` | shell | 1 to 120, default 30. |
| `args_pattern` | shell | A regex the whole argument must match (it is anchored). |
| `args_enum` | shell | The exact arguments allowed. |

A shell command declares at most one of `args_pattern` and `args_enum`.
Without either it takes no arguments, and `{args}` needs one of them. An
argument never contains a line break.

`{out}` is a temp file path the mod picks for the run
(`agentbus-<message id>.png` in the temp directory). It is deleted after the
run, and for an `image` command also before it. An `image` command must use
`{out}` and write a non-empty PNG there, under 4 MiB, which is the most the
mod reads.

A machine runs shell commands only when it opts in: `AGENTBUS_ALLOW_SHELL=1`
in the environment Claude Code runs in. Without it the reply is "shell
commands are disabled".

`AGENTBUS_ALLOW_SHELL` is a guard rail, not a security boundary. It only
stops registry `shell` commands. An owner's slash passthrough (any `!command`
the registry doesn't define runs as that Claude Code `/command`) and `prompt`
commands reach the agent itself, and the agent can run shell commands when
asked to. Treat being an owner as having a shell on every machine with the
mod.

In a linked conversation where guests read along, the bridge refuses
`shell` (and so `image`) commands: run them from your DM with the bot or the
channel. Slash and prompt commands still work there.

## Examples

A slash command with a fixed prefix:

```yaml
# focus.yaml: !focus the parser runs /compact focus on the parser
description: Compact, keeping one topic
kind: slash
command: compact
args: "focus on {args}"
```

A prompt:

```yaml
# review.yaml: !review the last commit
description: Ask the agent for a review
kind: prompt
text: "Review {args} and report the three most important problems."
```

A shell command that takes free text, passed through `env`:

```yaml
# gitlog.yaml: !gitlog 20
description: Recent commits in the agent's directory
kind: shell
argv:
  linux: [/bin/sh, -c, 'git log --oneline -n "$AGENTBUS_ARGS"']
  darwin: [/bin/sh, -c, 'git log --oneline -n "$AGENTBUS_ARGS"']
  windows: ['C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe', -NoProfile, -NonInteractive, -Command, 'git log --oneline -n $env:AGENTBUS_ARGS']
env:
  AGENTBUS_ARGS: "{args}"
# Digits only: no quote, no whitespace and no leading -, so git gets one
# plain value even from Windows PowerShell.
args_pattern: "[0-9]{1,3}"
```

A shell command with a fixed list of arguments, as an argv element:

```yaml
# branch.yaml: !branch main
description: Switch the agent's branch
kind: shell
argv:
  linux: [/usr/bin/git, switch, "{args}"]
  darwin: [/usr/bin/git, switch, "{args}"]
  windows: ['C:\Program Files\Git\cmd\git.exe', switch, "{args}"]
args_enum: [main, dev]
```

Use absolute program paths, as above, so nothing earlier in `PATH` or in the
agent's working directory can stand in for the program. The same goes for a
script an interpreter runs: `-File 'C:\agent-tools\tool.ps1'`, not
`-File tool.ps1`.

## Passing values safely

The text after `!name` comes from Slack. The proxy (when it loads a file) and
the mod (before it runs one) refuse any definition that could hand it to
something that parses it as code. Both apply the same rules.

- **`{args}` and `{out}` are whole values.** Each may be a whole argv element
  or a whole `env` value, never part of one, and never the program. Only
  `AGENTBUS_*` variables may take one, so a placeholder can't land in
  `LD_PRELOAD`, `BASH_ENV` and the like.
- **Free text goes through `env`.** `{args}` may be an argv element only when
  the command declares `args_enum`, whose values the owner wrote. With
  `args_pattern`, pass it in an `AGENTBUS_*` variable.
- **No placeholder after an interpreter.** Once an argv element is an
  interpreter or a wrapper, no `{args}` or `{out}` may follow it. Examples:
  `sh`, `bash`, `cmd`, `powershell`, `pwsh`, `python`, `node`, `perl`, `env`,
  `sudo`, `ssh`, `wsl` and `xargs`; the full list is in
  `internal/slackbridge/commands.go`. An interpreter picks its script from its
  arguments: PowerShell runs its first bare argument as `-Command`, `sh x`
  runs the file `x`, and `python -m x` runs the module `x`. Names are compared
  the way Windows runs them: `C:\...\PowerShell.EXE.`, `python3.12` and
  `pwsh-preview` all count.
- **No script file as the program.** A program ending in `.bat`, `.cmd`,
  `.ps1`, `.vbs`, `.js`, `.wsf` or `.hta` is refused, because Windows runs it
  through an interpreter. Call the interpreter with the script instead, for
  example `['C:\Program Files\PowerShell\7\pwsh.exe', -NoProfile, -File,
  'C:\agent-tools\tool.ps1']`, with the values in `env`.
- **No `%AGENTBUS_` or `!AGENTBUS_` anywhere.** An argv element containing
  either, in any case, is refused whatever the program, because cmd may run
  from inside the script (see cmd below).

The rules can't see what a script does with a variable. Read it like this:

- **sh**: always `"$AGENTBUS_ARGS"`, in double quotes. Unquoted, the shell
  splits the value and expands its wildcards. Quoting is not enough on its
  own: a value starting with `-` is read as an option, and some options run
  commands (`--pre=`, `--upload-pack=`, `-exec`, `--checkpoint-action=`).
  Put `--` before it where the program supports that (`grep -r --
  "$AGENTBUS_ARGS" /var/log/app`), or make `args_pattern` refuse a leading
  `-` (for example `[a-z0-9][a-z0-9 ._]*`).
- **PowerShell**: pass `$env:AGENTBUS_ARGS` only to cmdlets, for example
  `Select-String -Pattern $env:AGENTBUS_ARGS C:\logs\app.log`. Windows
  PowerShell 5.1 re-splits a value it passes to an `.exe` at embedded double
  quotes, so a native program could get extra arguments. Let a native
  program read the variable from its own environment, or use an
  `args_pattern` that excludes `"`, whitespace and a leading `-` (as
  `gitlog` above does). Never pass the value to `Invoke-Expression`, `iex`,
  `[scriptblock]::Create(...)` or `powershell -Command`,
  which run it as code.
- **cmd**: never reference the variable. cmd expands `%VAR%` (and `!VAR!`
  with delayed expansion) before it parses the line, so the value would be
  parsed as cmd syntax. That includes a `cmd /c ...` that a PowerShell or sh
  script starts, and `forfiles /c`. Let the program cmd starts read the
  variable itself, or use PowerShell with a cmdlet.
