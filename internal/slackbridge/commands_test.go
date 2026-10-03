package slackbridge

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// writeCommand writes dir/<file> and stamps it with mtime, so a test can
// change a file without waiting for the clock.
func writeCommand(t *testing.T, dir, file, content string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

var mtime0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

const (
	slashYAML  = "description: Compact with a focus\nkind: slash\ncommand: compact\nargs: \"focus on {args}\"\n"
	promptYAML = "description: Review the diff\nkind: prompt\ntext: \"Review {args} and report back.\"\n"
	shellYAML  = `description: Screenshot
kind: shell
argv:
  windows: [powershell, -NoProfile, -Command, "capture $env:AGENTBUS_OUT"]
  darwin: [screencapture, -x, "{out}"]
env:
  AGENTBUS_OUT: "{out}"
output: image
`
	gitLogYAML = `description: Recent commits
kind: shell
argv:
  linux: [git, log, --oneline, -n, "{args}"]
args_pattern: "[0-9]{1,3}"
`
	branchYAML = `description: Switch branch
kind: shell
argv:
  linux: [git, switch, "{args}"]
args_enum: [main, dev]
`
)

func TestRegistryLoadsEachKind(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "focus.yaml", slashYAML, mtime0)
	writeCommand(t, dir, "review.yaml", promptYAML, mtime0)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	writeCommand(t, dir, "notes.txt", "ignored", mtime0)
	if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := newRegistry(dir)

	e, ok := r.lookup("focus")
	if !ok || e.err != nil || e.spec.Kind != "slash" || e.spec.Command != "compact" || e.spec.Args != "focus on {args}" || e.spec.Description != "Compact with a focus" {
		t.Fatalf("focus = %+v %v", e, ok)
	}
	e, ok = r.lookup("review")
	if !ok || e.err != nil || e.spec.Kind != "prompt" || e.spec.Text != "Review {args} and report back." {
		t.Fatalf("review = %+v %v", e, ok)
	}
	e, ok = r.lookup("screenshot")
	if !ok || e.err != nil || e.spec.Kind != "shell" || e.spec.Output != "image" || e.spec.Timeout != 30 {
		t.Fatalf("screenshot = %+v %v", e, ok)
	}
	if want := []string{"screencapture", "-x", "{out}"}; !reflect.DeepEqual(e.spec.Argv["darwin"], want) {
		t.Fatalf("argv = %v", e.spec.Argv)
	}
	for _, missing := range []string{"notes", "compact"} {
		if _, found := r.lookup(missing); found {
			t.Fatalf("%s should not be a registry command", missing)
		}
	}
	// A .yaml entry that isn't a regular file is misconfigured, so !sub
	// never falls through to a harness command.
	if e, found := r.lookup("sub"); !found || e.err == nil {
		t.Fatalf("sub.yaml directory = %+v %v", e, found)
	}
	var names []string
	for _, entry := range r.list() {
		names = append(names, entry.name)
	}
	if !reflect.DeepEqual(names, []string{"focus", "review", "screenshot", "sub"}) {
		t.Fatalf("list = %v", names)
	}
}

func TestRegistryUnreadableEntriesAreMisconfigured(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	dir := t.TempDir()
	writeCommand(t, dir, "focus.yaml", slashYAML, mtime0)
	writeCommand(t, dir, "locked.yaml", slashYAML, mtime0)
	r := newRegistry(dir)
	r.stat = func(path string) (os.FileInfo, error) {
		if filepath.Base(path) == "locked.yaml" {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}
	if e, found := r.lookup("locked"); !found || e.err == nil {
		t.Fatalf("unstat-able file = %+v %v", e, found)
	}
	warned := false
	for _, e := range hook.AllEntries() {
		warned = warned || (e.Level == log.WarnLevel && strings.Contains(e.Message, "locked.yaml"))
	}
	if !warned {
		t.Fatal("no warning for the unreadable file")
	}
	if e, found := r.lookup("focus"); !found || e.err != nil {
		t.Fatalf("focus = %+v %v", e, found)
	}
}

func TestRegistryKeepsEntriesOnDirReadError(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	dir := t.TempDir()
	writeCommand(t, dir, "focus.yaml", slashYAML, mtime0)
	r := newRegistry(dir)
	if _, found := r.lookup("focus"); !found {
		t.Fatal("focus not loaded")
	}
	r.readDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }
	if e, found := r.lookup("focus"); !found || e.err != nil || e.spec.Command != "compact" {
		t.Fatalf("after a read error = %+v %v", e, found)
	}
	r.lookup("focus")
	warned := 0
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "agent commands") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("warned %d times, want 1", warned)
	}
	// The directory going away (ENOENT) still empties the registry.
	r.readDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist }
	if _, found := r.lookup("focus"); found {
		t.Fatal("a removed dir kept its commands")
	}
}

func TestCommandRepliesUseCommandQueue(t *testing.T) {
	b, _, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "broken.yaml", "kind: nope\n", mtime0)
	root := threadOf(t, b, bus)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	for i, ev := range []messageEvent{
		msg("UJANE", "!compact", "18.1", root),
		msg("UALEX", "!commands", "18.2", root),
		msg("UALEX", "!broken", "18.3", root),
		msg("UALEX", "!!!", "18.4", root),
		msg("UALEX", "!compact", "18.5", ""),
		msg("UALEX", "pc/other-bbbbbb: !compact", "18.6", ""),
		msg("UALEX", "ghost: !compact", "18.7", ""),
	} {
		b.handleEvent("EvQ"+string(rune('0'+i)), ev)
		if len(b.commands) != 1 || len(b.jobs) != 0 {
			t.Fatalf("event %d: commands=%d jobs=%d, want the reply on the command queue", i, len(b.commands), len(b.jobs))
		}
		drainJobs(t, b)
	}
}

func TestRegistryMissingOrEmptyDir(t *testing.T) {
	var nilRegistry *registry
	if _, ok := nilRegistry.lookup("x"); ok || len(nilRegistry.list()) != 0 {
		t.Fatal("a nil registry has commands")
	}
	r := newRegistry(filepath.Join(t.TempDir(), "absent"))
	if _, ok := r.lookup("x"); ok || len(r.list()) != 0 {
		t.Fatal("a missing dir has commands")
	}
	if newRegistry("") != nil {
		t.Fatal("an empty dir should mean no registry")
	}
}

func TestRegistryRejectsInvalidFiles(t *testing.T) {
	for name, content := range map[string]string{
		"nokind":       "description: x\ncommand: compact\n",
		"badkind":      "kind: bash\nargv:\n  linux: [ls]\n",
		"unknownkey":   "kind: slash\ncommand: compact\nshell: true\n",
		"slashnocmd":   "kind: slash\n",
		"slashbadcmd":  "kind: slash\ncommand: \"/compact now\"\n",
		"slashtext":    "kind: slash\ncommand: compact\ntext: hi\n",
		"promptnotext": "kind: prompt\ntext: \"  \"\n",
		"promptargv":   "kind: prompt\ntext: hi\nargv:\n  linux: [ls]\n",
		"shellnoargv":  "kind: shell\n",
		"shellbados":   "kind: shell\nargv:\n  plan9: [ls]\n",
		"shellempty":   "kind: shell\nargv:\n  linux: []\n",
		"embedargs":    "kind: shell\nargv:\n  linux: [git, \"--author={args}\"]\nargs_pattern: \"[a-z]+\"\n",
		"embedout":     "kind: shell\nargv:\n  linux: [cat, \"{out}.png\"]\n",
		"embedimage":   "kind: shell\nargv:\n  linux: [grim, \"{out}.png\"]\noutput: image\n",
		"embedboth":    "kind: shell\nargv:\n  linux: [sh, -c, \"echo {args}\"]\nargs_pattern: \"[a-z]+\"\n",
		"argsprogram":  "kind: shell\nargv:\n  linux: [\"{args}\"]\nargs_pattern: \"[a-z]+\"\n",
		"outprogram":   "kind: shell\nargv:\n  linux: [\"{out}\", x]\n",
		"badoutput":    "kind: shell\nargv:\n  linux: [ls]\noutput: html\n",
		"imagenoout":   "kind: shell\nargv:\n  linux: [ls]\noutput: image\n",
		"timeoutbig":   "kind: shell\nargv:\n  linux: [ls]\ntimeout_seconds: 121\n",
		"timeoutneg":   "kind: shell\nargv:\n  linux: [ls]\ntimeout_seconds: -1\n",
		"badpattern":   "kind: shell\nargv:\n  linux: [ls, \"{args}\"]\nargs_pattern: \"[\"\n",
		"bothrules":    "kind: shell\nargv:\n  linux: [ls, \"{args}\"]\nargs_pattern: \"[a-z]+\"\nargs_enum: [a]\n",
		"slashrule":    "kind: slash\ncommand: compact\nargs_enum: [a]\n",
		"emptyfile":    "",
		"notyaml":      "kind: [slash\n",
		"twodocs":      "kind: slash\ncommand: compact\n---\nkind: slash\ncommand: clear\n",
		// Wrapped as ^(?:a)|(.*)$ this would be an unanchored alternation.
		"patternescape": "kind: shell\nargv:\n  linux: [ls, \"{args}\"]\nargs_pattern: \"a)|(.*\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeCommand(t, dir, name+".yaml", content, mtime0)
			e, ok := newRegistry(dir).lookup(name)
			if !ok || e.err == nil {
				t.Fatalf("%s loaded: %+v", content, e.spec)
			}
		})
	}
}

func TestRegistryAcceptsPlaceholdersAsWholeElements(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "gitlog.yaml", gitLogYAML, mtime0)
	writeCommand(t, dir, "branch.yaml", branchYAML, mtime0)
	writeCommand(t, dir, "outtext.yaml", "kind: shell\nargv:\n  linux: [cat, \"{out}\"]\ntimeout_seconds: 120\n", mtime0)
	r := newRegistry(dir)
	for _, name := range []string{"gitlog", "branch", "outtext"} {
		if e, ok := r.lookup(name); !ok || e.err != nil {
			t.Fatalf("%s = %+v %v", name, e, ok)
		}
	}
	if e, _ := r.lookup("outtext"); e.spec.Output != "text" || e.spec.Timeout != 120 {
		t.Fatalf("defaults = %+v", e.spec)
	}
}

// An interpreter never gets {args} or {out} as an argument: an interpreter
// picks its script from its arguments, so they go through env. A script file
// as the program is refused outright. {args} anywhere needs an argument rule.
func TestRegistryKeepsPlaceholdersAwayFromInterpreters(t *testing.T) {
	const rule = "args_pattern: \"[a-z]+\"\n"
	for name, content := range map[string]string{
		"shc":         "kind: shell\nargv:\n  linux: [sh, -c, \"{args}\"]\n" + rule,
		"shcafter":    "kind: shell\nargv:\n  linux: [sh, -c, \"echo $1\", sh, \"{args}\"]\n" + rule,
		"bashlc":      "kind: shell\nargv:\n  linux: [/bin/bash, -lc, echo, \"{args}\"]\n" + rule,
		"zshc":        "kind: shell\nargv:\n  linux: [zsh, -c, x, \"{out}\"]\n",
		"dashc":       "kind: shell\nargv:\n  linux: [dash, -c, x, \"{out}\"]\n",
		"kshc":        "kind: shell\nargv:\n  linux: [ksh, -c, x, \"{out}\"]\n",
		"fishcommand": "kind: shell\nargv:\n  linux: [fish, --command, x, \"{out}\"]\n",
		"pwshcommand": "kind: shell\nargv:\n  windows: [powershell.exe, -NoProfile, -Command, capture, \"{out}\"]\noutput: image\n",
		"pwshlower":   "kind: shell\nargv:\n  windows: [pwsh, -command, \"{out}\"]\n",
		"pwshabbrev":  "kind: shell\nargv:\n  windows: [pwsh, -com, x, \"{out}\"]\n",
		"pwshenc":     "kind: shell\nargv:\n  windows: [PowerShell, -EncodedCommand, \"{args}\"]\n" + rule,
		"pwshe":       "kind: shell\nargv:\n  windows: [pwsh, -e, \"{args}\"]\n" + rule,
		"pwshslash":   "kind: shell\nargv:\n  windows: [powershell, /Command, \"{args}\"]\n" + rule,
		"cmdc":        "kind: shell\nargv:\n  windows: [cmd, /c, dir, \"{args}\"]\n" + rule,
		"cmdupper":    "kind: shell\nargv:\n  windows: ['C:\\Windows\\System32\\CMD.EXE', /C, \"{out}\"]\n",
		"cmdk":        "kind: shell\nargv:\n  windows: [cmd.exe, /k, \"{out}\"]\n",
		"pythonc":     "kind: shell\nargv:\n  linux: [python3, -c, \"import sys\", \"{args}\"]\n" + rule,
		"pythonv":     "kind: shell\nargv:\n  linux: [/usr/bin/python3.12, -c, x, \"{args}\"]\n" + rule,
		"nodee":       "kind: shell\nargv:\n  linux: [node, -e, x, \"{args}\"]\n" + rule,
		"nodeeval":    "kind: shell\nargv:\n  linux: [node, --eval, x, \"{args}\"]\n" + rule,
		"perle":       "kind: shell\nargv:\n  linux: [perl, -E, x, \"{args}\"]\n" + rule,
		"rubye":       "kind: shell\nargv:\n  linux: [ruby, -e, x, \"{args}\"]\n" + rule,
		"osascripte":  "kind: shell\nargv:\n  darwin: [osascript, -e, \"{args}\"]\n" + rule,
		"wrapped":     "kind: shell\nargv:\n  linux: [env, sh, -c, x, \"{args}\"]\n" + rule,
		"bat":         "kind: shell\nargv:\n  windows: [run.bat, \"{args}\"]\n" + rule,
		"cmdfile":     "kind: shell\nargv:\n  windows: ['C:\\tools\\Run.CMD']\n",
		"argsnorule":  "kind: shell\nargv:\n  linux: [git, show, \"{args}\"]\n",
		"envnorule":   "kind: shell\nargv:\n  linux: [tool]\nenv:\n  AGENTBUS_ARGS: \"{args}\"\n",
		"envembedded": "kind: shell\nargv:\n  linux: [tool]\nenv:\n  AGENTBUS_ARGS: \"x {args}\"\n" + rule,
		"envembedout": "kind: shell\nargv:\n  linux: [tool]\nenv:\n  AGENTBUS_OUT: \"{out}.png\"\n",
		"envbadkey":   "kind: shell\nargv:\n  linux: [tool]\nenv:\n  \"1A\": x\n",
		"envpreload":  "kind: shell\nargv:\n  linux: [tool]\nenv:\n  LD_PRELOAD: \"{args}\"\n" + rule,
		"envslash":    "kind: slash\ncommand: compact\nenv:\n  A: b\n",
		"envprompt":   "kind: prompt\ntext: hi\nenv:\n  A: b\n",
		// No flag needed: an interpreter picks its script from its arguments.
		"pwshbare":     "kind: shell\nargv:\n  windows: [powershell, -NoProfile, \"{args}\"]\n" + rule,
		"pwshonly":     "kind: shell\nargv:\n  windows: [pwsh, \"{args}\"]\n" + rule,
		"pwshfile":     "kind: shell\nargv:\n  windows: [pwsh, -NoProfile, -File, tool.ps1, \"{args}\"]\n" + rule,
		"pythonm":      "kind: shell\nargv:\n  linux: [python, -m, \"{args}\"]\n" + rule,
		"pythonscript": "kind: shell\nargv:\n  linux: [python3, tool.py, \"{args}\"]\n" + rule,
		"shbare":       "kind: shell\nargv:\n  linux: [sh, \"{args}\"]\n" + rule,
		"envbare":      "kind: shell\nargv:\n  linux: [env, \"{args}\"]\n" + rule,
		"nodeout":      "kind: shell\nargv:\n  linux: [node, x.js, \"{out}\"]\n",
		"pyexe":        "kind: shell\nargv:\n  windows: [py.exe, \"{args}\"]\n" + rule,
		"pythonwcaps":  "kind: shell\nargv:\n  windows: ['C:\\Python\\PYTHONW.EXE', x.py, \"{out}\"]\n",
		"pythonver":    "kind: shell\nargv:\n  linux: [python3.12, x.py, \"{out}\"]\n",
		"cmdcom":       "kind: shell\nargv:\n  windows: [cmd.com, \"{out}\"]\n",
		"wsl":          "kind: shell\nargv:\n  windows: [wsl, \"{args}\"]\n" + rule,
		"busybox":      "kind: shell\nargv:\n  linux: [busybox, sh, \"{args}\"]\n" + rule,
		"awk":          "kind: shell\nargv:\n  linux: [gawk, -f, x.awk, \"{out}\"]\n",
		"sed":          "kind: shell\nargv:\n  linux: [sed, -n, p, \"{out}\"]\n",
		"ssh":          "kind: shell\nargv:\n  linux: [ssh, host, \"{args}\"]\n" + rule,
		"xargs":        "kind: shell\nargv:\n  linux: [xargs, \"{args}\"]\n" + rule,
		"mshta":        "kind: shell\nargv:\n  windows: [mshta, \"{args}\"]\n" + rule,
		"rscript":      "kind: shell\nargv:\n  linux: [Rscript, x.R, \"{out}\"]\n",
		"sudowrapped":  "kind: shell\nargv:\n  linux: [sudo, sh, \"{args}\"]\n" + rule,
		"ps1":          "kind: shell\nargv:\n  windows: ['C:\\tools\\shot.PS1']\n",
		"vbs":          "kind: shell\nargv:\n  windows: [shot.vbs]\n",
		"js":           "kind: shell\nargv:\n  windows: [shot.js]\n",
		"wsf":          "kind: shell\nargv:\n  windows: [shot.wsf]\n",
		"hta":          "kind: shell\nargv:\n  windows: [shot.hta]\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeCommand(t, dir, name+".yaml", content, mtime0)
			e, ok := newRegistry(dir).lookup(name)
			if !ok || e.err == nil {
				t.Fatalf("%s loaded: %+v", content, e.spec)
			}
		})
	}
}

func TestRegistryAcceptsEnvPassedPlaceholders(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "gnome.yaml", "kind: shell\nargv:\n  linux: [sh, -c, 'gnome-screenshot -f \"$AGENTBUS_OUT\"']\nenv:\n  AGENTBUS_OUT: \"{out}\"\n  LANG: C\noutput: image\n", mtime0)
	writeCommand(t, dir, "grep.yaml", "kind: shell\nargv:\n  windows: [powershell, -NoProfile, -Command, 'Select-String -Pattern $env:AGENTBUS_ARGS x.log']\nenv:\n  AGENTBUS_ARGS: \"{args}\"\nargs_pattern: \"[a-z]+\"\n", mtime0)
	writeCommand(t, dir, "script.yaml", "kind: shell\nargv:\n  linux: [python3, tool.py]\n  windows: [pwsh, -NoProfile, -File, tool.ps1]\nenv:\n  AGENTBUS_ARGS: \"{args}\"\nargs_enum: [a, b]\n", mtime0)
	// Interpreter flags are fine without placeholders.
	writeCommand(t, dir, "strict.yaml", "kind: shell\nargv:\n  linux: [bash, -e, script.sh]\n  darwin: [sh, -ex, run.sh]\n", mtime0)
	// A program that isn't an interpreter takes placeholders as whole elements.
	writeCommand(t, dir, "capture.yaml", "kind: shell\nargv:\n  darwin: [screencapture, -x, \"{out}\"]\n  linux: [gnome-screenshot, -f, \"{out}\"]\noutput: image\n", mtime0)
	r := newRegistry(dir)
	for _, name := range []string{"gnome", "grep", "script", "strict", "capture"} {
		if e, ok := r.lookup(name); !ok || e.err != nil {
			t.Fatalf("%s = %+v %v", name, e, ok)
		}
	}
	e, _ := r.lookup("gnome")
	cmd := e.spec.command("")
	if want := map[string]string{"AGENTBUS_OUT": "{out}", "LANG": "C"}; !reflect.DeepEqual(cmd.Env, want) {
		t.Fatalf("env = %v, want %v", cmd.Env, want)
	}
	e, _ = r.lookup("grep")
	if cmd = e.spec.command("abc"); cmd.Env["AGENTBUS_ARGS"] != "{args}" || cmd.Args != "abc" {
		t.Fatalf("grep command = %+v", cmd)
	}
}

func TestRegistryRejectsBadFilenames(t *testing.T) {
	dir := t.TempDir()
	for _, file := range []string{"Upper.yaml", "-dash.yaml", "has space.yaml", strings.Repeat("a", 33) + ".yaml", "commands.yaml"} {
		writeCommand(t, dir, file, slashYAML, mtime0)
	}
	r := newRegistry(dir)
	for _, entry := range r.list() {
		if entry.err == nil {
			t.Fatalf("loaded %q", entry.name)
		}
	}
	if e, ok := r.lookup("upper"); ok && e.err == nil {
		t.Fatal("Upper.yaml loaded as upper")
	}
}

func TestRegistryReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "focus.yaml", slashYAML, mtime0)
	r := newRegistry(dir)
	if e, ok := r.lookup("focus"); !ok || e.spec.Command != "compact" {
		t.Fatalf("focus = %+v", e)
	}
	// Same size, new mtime: reloaded.
	writeCommand(t, dir, "focus.yaml", strings.Replace(slashYAML, "compact", "cleared", 1), mtime0.Add(time.Second))
	if e, ok := r.lookup("focus"); !ok || e.spec.Command != "cleared" {
		t.Fatalf("after edit = %+v", e)
	}
	// New file.
	writeCommand(t, dir, "review.yaml", promptYAML, mtime0)
	if _, ok := r.lookup("review"); !ok {
		t.Fatal("new file not picked up")
	}
	// Removed file.
	if err := os.Remove(filepath.Join(dir, "focus.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.lookup("focus"); ok {
		t.Fatal("removed file still listed")
	}
	// Directory removed entirely.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if len(r.list()) != 0 {
		t.Fatal("removed dir still has commands")
	}
}

func TestRegistryWarnsOncePerChange(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	dir := t.TempDir()
	writeCommand(t, dir, "broken.yaml", "kind: bash\n", mtime0)
	r := newRegistry(dir)
	warns := func() int {
		n := 0
		for _, e := range hook.AllEntries() {
			if e.Level == log.WarnLevel && strings.Contains(e.Message, "broken.yaml") {
				n++
			}
		}
		return n
	}
	r.lookup("broken")
	r.lookup("broken")
	writeCommand(t, dir, "other.yaml", promptYAML, mtime0) // reload, broken.yaml unchanged
	r.lookup("broken")
	if n := warns(); n != 1 {
		t.Fatalf("warned %d times for one broken version", n)
	}
	writeCommand(t, dir, "broken.yaml", "kind: shell\n", mtime0.Add(time.Second))
	r.lookup("broken")
	if n := warns(); n != 2 {
		t.Fatalf("warned %d times after a change, want 2", n)
	}
}

func TestShellArgsRules(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "gitlog.yaml", gitLogYAML, mtime0)
	writeCommand(t, dir, "branch.yaml", branchYAML, mtime0)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	writeCommand(t, dir, "review.yaml", promptYAML, mtime0)
	writeCommand(t, dir, "anytext.yaml", "kind: shell\nargv:\n  linux: [echo, \"{args}\"]\nargs_pattern: \"(?s).{1,20}\"\n", mtime0)
	writeCommand(t, dir, "focus.yaml", slashYAML, mtime0)
	writeCommand(t, dir, "opus.yaml", "kind: slash\ncommand: model\nargs: opus\n", mtime0)
	writeCommand(t, dir, "raw.yaml", "kind: slash\ncommand: model\n", mtime0)
	r := newRegistry(dir)
	spec := func(name string) *commandSpec {
		e, _ := r.lookup(name)
		return e.spec
	}
	for _, tc := range []struct {
		name, rest string
		ok         bool
	}{
		{"screenshot", "", true},
		{"screenshot", "x", false},
		{"gitlog", "10", true},
		{"gitlog", "1000", false},
		{"gitlog", "10; rm -rf /", false},
		{"gitlog", "", false},
		{"branch", "main", true},
		{"branch", "dev", true},
		{"branch", "main dev", false},
		{"branch", "", false},
		{"review", "anything at all; $(x)", true},
		// A shell argument never spans lines, whatever the pattern allows.
		{"anytext", "a b", true},
		{"anytext", "a\nb", false},
		{"anytext", "a\rb", false},
		// A slash template without {args} takes no arguments.
		{"opus", "", true},
		{"opus", "haiku", false},
		{"focus", "tests", true},
		{"raw", "anything", true},
	} {
		if err := spec(tc.name).checkArgs(tc.rest); (err == nil) != tc.ok {
			t.Fatalf("%s %q: err = %v, want ok=%v", tc.name, tc.rest, err, tc.ok)
		}
	}
	if err := spec("screenshot").checkArgs("x"); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("no-args refusal = %v", err)
	}
	if err := spec("gitlog").checkArgs("x"); err == nil || !strings.Contains(err.Error(), "invalid arguments for `!gitlog`") {
		t.Fatalf("pattern refusal = %v", err)
	}
	if err := spec("opus").checkArgs("haiku"); err == nil || !strings.Contains(err.Error(), "`!opus` takes no arguments") {
		t.Fatalf("slash template refusal = %v", err)
	}
}

func TestParseBang(t *testing.T) {
	for _, tc := range []struct {
		text, name, rest string
		ok               bool
	}{
		{"!compact", "compact", "", true},
		{"  !Compact  ", "compact", "", true},
		{"!model  opus  ", "model", "opus", true},
		{"!rename my\nnew name", "rename", "my\nnew name", true},
		{"!plugin:cmd x", "plugin:cmd", "x", true},
		{"!!!", "", "", false},
		{"! compact", "", "", false},
		{"!-x", "", "", false},
		{"!" + strings.Repeat("a", 65), "", "", false},
	} {
		name, rest, err := parseBang(tc.text)
		if (err == nil) != tc.ok || name != tc.name || rest != tc.rest {
			t.Fatalf("parseBang(%q) = %q, %q, %v", tc.text, name, rest, err)
		}
	}
	if _, _, err := parseBang("!x " + strings.Repeat("é", maxCommandArgs+1)); err == nil {
		t.Fatal("over-long args accepted")
	}
	if _, rest, err := parseBang("!x " + strings.Repeat("é", maxCommandArgs)); err != nil || len([]rune(rest)) != maxCommandArgs {
		t.Fatalf("args at the cap: %v", err)
	}
}

// newCommandBridge is newTestBridge with a registry in a temp dir and sidA
// (flyer) running a command-capable mod. sidB stays without a mod version.
func newCommandBridge(t *testing.T) (*Bridge, *fakeSlack, *agentbus.Store, string) {
	t.Helper()
	b, f, bus := newTestBridge(t)
	dir := t.TempDir()
	b.cmdRegistry = newRegistry(dir)
	bus.SetModVersion(sidA, "0.3.3")
	return b, f, bus, dir
}

func reactions(f *fakeSlack) []string {
	var out []string
	for _, c := range f.callsTo("reactions.add") {
		out = append(out, c.Form.Get("name"))
	}
	return out
}

func TestOwnerCommandInThreadIsDelivered(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvC1", msg("UALEX", "!compact", "10.1", root))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Command == nil {
		t.Fatalf("msgs = %+v", msgs)
	}
	m := msgs[0]
	want := agentbus.Command{Name: "compact", Kind: "slash", Command: "compact"}
	if !reflect.DeepEqual(*m.Command, want) || !m.FromUser || m.SlackUser != "alex" || m.SlackUserID != "UALEX" {
		t.Fatalf("msg = %+v cmd = %+v", m, *m.Command)
	}
	drainJobs(t, b)
	if got := reactions(f); !reflect.DeepEqual(got, []string{"gear"}) {
		t.Fatalf("reactions = %v", got)
	}
	// The mod's result goes to the thread the command came from.
	if _, err := bus.Send(sidA, "slack", "done", m.ID); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if last := posts[len(posts)-1].Form; last.Get("thread_ts") != root || last.Get("text") != "done" {
		t.Fatalf("result post = %v", last)
	}
}

func TestHarnessCommandFallsThroughWithArgs(t *testing.T) {
	b, _, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvC2", msg("UALEX", "!Model claude-opus <@UALEX>", "10.2", root))
	msgs := bus.Claim(sidA)
	want := agentbus.Command{Name: "model", Kind: "slash", Command: "model", Args: "claude-opus @alex"}
	if len(msgs) != 1 || msgs[0].Command == nil || !reflect.DeepEqual(*msgs[0].Command, want) {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestTopLevelAddressedCommand(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	b.handleEvent("EvC3", msg("UALEX", "flyer: !clear", "10.3", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Command == nil || msgs[0].Command.Command != "clear" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if ts, _ := b.state.thread(sidA); ts != "10.3" {
		t.Fatalf("thread = %q", ts)
	}
	drainJobs(t, b)
	if got := reactions(f); !reflect.DeepEqual(got, []string{"gear"}) {
		t.Fatalf("reactions = %v", got)
	}
}

func TestRegistryCommandShadowsHarness(t *testing.T) {
	b, _, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "compact.yaml", promptYAML, mtime0)
	writeCommand(t, dir, "focus.yaml", slashYAML, mtime0)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	b.handleEvent("EvC4", msg("UALEX", "flyer: !compact the auth module", "10.4", ""))
	b.handleEvent("EvC5", msg("UALEX", "flyer: !focus tests", "10.5", ""))
	b.handleEvent("EvC6", msg("UALEX", "flyer: !screenshot", "10.6", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 3 {
		t.Fatalf("msgs = %+v", msgs)
	}
	prompt := agentbus.Command{Name: "compact", Kind: "prompt", Text: "Review {args} and report back.", Args: "the auth module"}
	if !reflect.DeepEqual(*msgs[0].Command, prompt) {
		t.Fatalf("prompt = %+v", *msgs[0].Command)
	}
	// A slash template gets {args} filled here; the mod runs it as given.
	slash := agentbus.Command{Name: "focus", Kind: "slash", Command: "compact", Args: "focus on tests"}
	if !reflect.DeepEqual(*msgs[1].Command, slash) {
		t.Fatalf("slash = %+v", *msgs[1].Command)
	}
	shell := agentbus.Command{Name: "screenshot", Kind: "shell", Output: "image", Timeout: 30, Argv: map[string][]string{
		"windows": {"powershell", "-NoProfile", "-Command", "capture $env:AGENTBUS_OUT"},
		"darwin":  {"screencapture", "-x", "{out}"},
	}, Env: map[string]string{"AGENTBUS_OUT": "{out}"}}
	if !reflect.DeepEqual(*msgs[2].Command, shell) {
		t.Fatalf("shell = %+v", *msgs[2].Command)
	}
}

func TestShellArgsRefusedAtDelivery(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	writeCommand(t, dir, "gitlog.yaml", gitLogYAML, mtime0)
	b.handleEvent("EvS1", msg("UALEX", "flyer: !screenshot now", "11.1", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "`!screenshot` takes no arguments") {
		t.Fatalf("reply = %q", got)
	}
	b.handleEvent("EvS2", msg("UALEX", "flyer: !gitlog 5; rm -rf /", "11.2", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "invalid arguments for `!gitlog`") {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidA) {
		t.Fatalf("refused shell command delivered: %+v", bus.Claim(sidA))
	}
	b.handleEvent("EvS3", msg("UALEX", "flyer: !gitlog 5", "11.3", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Command.Args != "5" || !reflect.DeepEqual(msgs[0].Command.Argv["linux"], []string{"git", "log", "--oneline", "-n", "{args}"}) {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestMisconfiguredCommandNeverFallsThrough(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "compact.yaml", "kind: slash\ncommand: compact\nbogus: 1\n", mtime0)
	root := threadOf(t, b, bus)
	b.handleEvent("EvM1", msg("UALEX", "!compact", "12.1", root))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "`!compact` is misconfigured") {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidA) {
		t.Fatal("misconfigured command fell through to the harness")
	}
}

func TestNonOwnerCannotRunCommands(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"!compact", "!commands"} {
		b.handleEvent("EvN"+string(rune('0'+i)), msg("UJANE", text, "13."+string(rune('1'+i)), root))
		drainJobs(t, b)
		if got := lastPostText(f); got != "Only owners can run commands." {
			t.Fatalf("reply to %q = %q", text, got)
		}
	}
	b.handleEvent("EvN9", msg("UJANE", "flyer: !compact", "13.9", ""))
	drainJobs(t, b)
	if got := lastPostText(f); got != "Only owners can run commands." {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidA) {
		t.Fatalf("a non-owner's command was delivered: %+v", bus.Claim(sidA))
	}
	if b.IsOwner("UJANE") || !b.IsOwner("UALEX") || b.IsOwner("UEVE") {
		t.Fatal("IsOwner disagrees with the config users")
	}
}

func TestCommandsListing(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	writeCommand(t, dir, "review.yaml", promptYAML, mtime0)
	writeCommand(t, dir, "broken.yaml", "kind: nope\n", mtime0)
	root := threadOf(t, b, bus)
	for i, ev := range []messageEvent{
		msg("UALEX", "!commands", "14.1", root),
		msg("UALEX", "flyer: !commands", "14.2", ""),
		msg("UALEX", "!commands", "14.3", ""),
	} {
		b.handleEvent("EvL"+string(rune('0'+i)), ev)
		drainJobs(t, b)
		got := lastPostText(f)
		for _, want := range []string{"`!review` (prompt): Review the diff", "`!screenshot` (shell): Screenshot", "`!broken`: misconfigured", "Claude Code `/command`"} {
			if !strings.Contains(got, want) {
				t.Fatalf("listing %d lacks %q:\n%s", i, want, got)
			}
		}
		if strings.Index(got, "!broken") > strings.Index(got, "!review") {
			t.Fatalf("listing not sorted:\n%s", got)
		}
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("!commands reached an agent")
	}
	if len(reactions(f)) != 0 {
		t.Fatal("!commands was reacted to as a delivery")
	}
}

func TestCommandsListingWithoutRegistry(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("EvL9", msg("UALEX", "!commands", "14.9", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "No registry commands") || !strings.Contains(got, "Claude Code `/command`") {
		t.Fatalf("listing = %q", got)
	}
}

func TestCommandToIncapableSessionIsRefused(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	b.handleEvent("EvI1", msg("UALEX", "pc/other-bbbbbb: !compact", "15.1", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "`pc/other-bbbbbb` can't run commands (agentbus plugin 0.3.3+ required)") {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidB) {
		t.Fatal("delivered to an incapable session")
	}
	b.handleEvent("EvI2", msg("UALEX", "ghost: !compact", "15.2", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "No agent called `ghost`") {
		t.Fatalf("reply = %q", got)
	}
}

func TestInvalidCommandNameGetsHelp(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvB1", msg("UALEX", "!!! urgent", "16.1", root))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "isn't a command") {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidA) {
		t.Fatal("delivered")
	}
}

func TestPlainMessagesStillDeliveredAsText(t *testing.T) {
	b, _, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvP1", msg("UALEX", "run !compact later", "17.1", root))
	b.handleEvent("EvP2", msg("UALEX", "flyer: tell me about !compact", "17.2", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 2 || msgs[0].Command != nil || msgs[1].Command != nil {
		t.Fatalf("msgs = %+v", msgs)
	}
}
