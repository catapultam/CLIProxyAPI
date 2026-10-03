package slackbridge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	// maxCommandArgs caps the text after "!name", in characters.
	maxCommandArgs      = 2000
	maxCommandFileSize  = 256 << 10
	defaultShellTimeout = 30
	maxShellTimeout     = 120
	argsPlaceholder     = "{args}"
	outPlaceholder      = "{out}"
	commandFileExt      = ".yaml"
	// listCommandsName is the bridge's own command; a registry file can't
	// take it.
	listCommandsName = "commands"
)

var (
	// commandFileName is a registry command's name, from its file name.
	commandFileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// commandName is what may follow "!": a registry name or a Claude Code
	// slash command (plugin commands carry a colon).
	commandName = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,63}$`)
	shellOSKeys = map[string]bool{"windows": true, "darwin": true, "linux": true}
	// shellEnvName is an env variable a shell command may set.
	shellEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// shellEnvPrefix is the prefix of the env variables that may carry {args}
// or {out}.
const shellEnvPrefix = "AGENTBUS_"

// commandFile is one <name>.yaml in the registry. Unknown keys are rejected.
type commandFile struct {
	Description    string              `yaml:"description"`
	Kind           string              `yaml:"kind"`
	Command        string              `yaml:"command"`
	Args           string              `yaml:"args"`
	Text           string              `yaml:"text"`
	Argv           map[string][]string `yaml:"argv"`
	Env            map[string]string   `yaml:"env"`
	Output         string              `yaml:"output"`
	TimeoutSeconds int                 `yaml:"timeout_seconds"`
	ArgsPattern    string              `yaml:"args_pattern"`
	ArgsEnum       []string            `yaml:"args_enum"`
}

// commandSpec is a validated registry command.
type commandSpec struct {
	Name        string
	Description string
	Kind        string
	Command     string
	Args        string
	Text        string
	Argv        map[string][]string
	Env         map[string]string
	Output      string
	Timeout     int
	// argsPattern (anchored) or argsEnum limits a shell command's
	// arguments. With neither, a shell command takes no arguments.
	argsPattern *regexp.Regexp
	argsEnum    []string
}

// checkArgs reports why rest isn't acceptable for c, as a reply to the
// owner. Prompt commands, and slash commands whose args template uses
// {args} (or that have none), take free-form arguments. A slash template
// without {args}, and a shell command without args_pattern or args_enum,
// take none.
func (c *commandSpec) checkArgs(rest string) error {
	noArgs := fmt.Errorf("`!%s` takes no arguments.", c.Name)
	switch c.Kind {
	case agentbus.CommandSlash:
		if c.Args != "" && !strings.Contains(c.Args, argsPlaceholder) && rest != "" {
			return noArgs
		}
		return nil
	case agentbus.CommandShell:
	default:
		return nil
	}
	invalid := fmt.Errorf("invalid arguments for `!%s`.", c.Name)
	if strings.ContainsAny(rest, "\r\n") {
		return invalid
	}
	switch {
	case c.argsPattern != nil:
		if !c.argsPattern.MatchString(rest) {
			return invalid
		}
	case len(c.argsEnum) > 0:
		if !slices.Contains(c.argsEnum, rest) {
			return invalid
		}
	case rest != "":
		return noArgs
	}
	return nil
}

// command builds what the mod runs for c with the owner's arguments rest.
// A slash template's {args} is filled here, so the mod runs Command and
// Args as given; prompt and shell keep their templates, and the mod puts
// Args in place of {args}.
func (c *commandSpec) command(rest string) agentbus.Command {
	cmd := agentbus.Command{Name: c.Name, Kind: c.Kind}
	switch c.Kind {
	case agentbus.CommandSlash:
		cmd.Command = c.Command
		cmd.Args = rest
		if c.Args != "" {
			cmd.Args = strings.ReplaceAll(c.Args, argsPlaceholder, rest)
		}
	case agentbus.CommandPrompt:
		cmd.Text = c.Text
		cmd.Args = rest
	case agentbus.CommandShell:
		cmd.Argv = c.Argv
		cmd.Env = c.Env
		cmd.ArgsEnum = c.argsEnum
		cmd.Output = c.Output
		cmd.Timeout = c.Timeout
		cmd.Args = rest
	}
	return cmd
}

// parseCommandFile decodes and validates one registry file.
func parseCommandFile(name string, data []byte) (*commandSpec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f commandFile
	if errDecode := dec.Decode(&f); errDecode != nil {
		if errors.Is(errDecode, io.EOF) {
			return nil, errors.New("empty file")
		}
		return nil, errDecode
	}
	var extra yaml.Node
	if errExtra := dec.Decode(&extra); !errors.Is(errExtra, io.EOF) {
		return nil, errors.New("more than one YAML document")
	}
	spec := &commandSpec{Name: name, Description: strings.TrimSpace(f.Description), Kind: f.Kind}
	unused := func(fields map[string]bool) error {
		for field, set := range fields {
			if set {
				return fmt.Errorf("%s is not used by kind %s", field, f.Kind)
			}
		}
		return nil
	}
	shellOnly := map[string]bool{
		"argv": f.Argv != nil, "env": f.Env != nil, "output": f.Output != "", "timeout_seconds": f.TimeoutSeconds != 0,
		"args_pattern": f.ArgsPattern != "", "args_enum": f.ArgsEnum != nil,
	}
	switch f.Kind {
	case agentbus.CommandSlash:
		shellOnly["text"] = f.Text != ""
		if errUnused := unused(shellOnly); errUnused != nil {
			return nil, errUnused
		}
		if !commandName.MatchString(f.Command) {
			return nil, errors.New("command must be a slash command name without the /")
		}
		spec.Command, spec.Args = f.Command, f.Args
	case agentbus.CommandPrompt:
		shellOnly["command"], shellOnly["args"] = f.Command != "", f.Args != ""
		if errUnused := unused(shellOnly); errUnused != nil {
			return nil, errUnused
		}
		if strings.TrimSpace(f.Text) == "" {
			return nil, errors.New("text is required")
		}
		spec.Text = f.Text
	case agentbus.CommandShell:
		if errUnused := unused(map[string]bool{"command": f.Command != "", "args": f.Args != "", "text": f.Text != ""}); errUnused != nil {
			return nil, errUnused
		}
		if errShell := parseShell(&f, spec); errShell != nil {
			return nil, errShell
		}
	default:
		return nil, errors.New("kind must be slash, prompt or shell")
	}
	return spec, nil
}

// parseShell validates a shell command's argv, output, timeout and argument
// rule into spec.
func parseShell(f *commandFile, spec *commandSpec) error {
	spec.Output = f.Output
	if spec.Output == "" {
		spec.Output = "text"
	}
	if spec.Output != "text" && spec.Output != "image" {
		return errors.New("output must be text or image")
	}
	spec.Timeout = f.TimeoutSeconds
	if spec.Timeout == 0 {
		spec.Timeout = defaultShellTimeout
	}
	if spec.Timeout < 1 || spec.Timeout > maxShellTimeout {
		return fmt.Errorf("timeout_seconds must be 1-%d", maxShellTimeout)
	}
	if len(f.Argv) == 0 {
		return errors.New("argv is required")
	}
	// usesArgs: {args} anywhere; argvArgs: {args} as an argv element.
	usesArgs, argvArgs, envOut := false, false, false
	for name, value := range f.Env {
		if !shellEnvName.MatchString(name) {
			return fmt.Errorf("env name %q must be letters, digits and _ (not starting with a digit)", name)
		}
		switch {
		case value == argsPlaceholder || value == outPlaceholder:
			// Only the mod's own variables carry Slack text or the temp path,
			// so a placeholder can't land in LD_PRELOAD, BASH_ENV and the like.
			if !strings.HasPrefix(name, shellEnvPrefix) {
				return fmt.Errorf("env.%s: {args} and {out} only go in %s* variables", name, shellEnvPrefix)
			}
			usesArgs = usesArgs || value == argsPlaceholder
			envOut = envOut || value == outPlaceholder
		case strings.Contains(value, argsPlaceholder) || strings.Contains(value, outPlaceholder):
			return fmt.Errorf("env.%s: {args} and {out} must be the whole value", name)
		}
	}
	spec.Env = maps.Clone(f.Env)
	spec.Argv = make(map[string][]string, len(f.Argv))
	for osKey, argv := range f.Argv {
		if !shellOSKeys[osKey] {
			return fmt.Errorf("argv key %q must be windows, darwin or linux", osKey)
		}
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return fmt.Errorf("argv.%s needs a program", osKey)
		}
		for i, el := range argv {
			if el == argsPlaceholder || el == outPlaceholder {
				if i == 0 {
					return fmt.Errorf("argv.%s: %s can't be the program", osKey, el)
				}
				if el == argsPlaceholder {
					usesArgs, argvArgs = true, true
				}
				continue
			}
			if strings.Contains(el, argsPlaceholder) || strings.Contains(el, outPlaceholder) {
				return fmt.Errorf("argv.%s: {args} and {out} must be whole argv elements", osKey)
			}
		}
		if errUnsafe := unsafeShellArgv(argv); errUnsafe != nil {
			return fmt.Errorf("argv.%s: %w", osKey, errUnsafe)
		}
		if spec.Output == "image" && !envOut && !slices.Contains(argv, outPlaceholder) {
			return fmt.Errorf("argv.%s: output image needs an {out} element or env value", osKey)
		}
		spec.Argv[osKey] = append([]string(nil), argv...)
	}
	if f.ArgsPattern != "" && len(f.ArgsEnum) > 0 {
		return errors.New("declare args_pattern or args_enum, not both")
	}
	if usesArgs && f.ArgsPattern == "" && len(f.ArgsEnum) == 0 {
		return errors.New("{args} needs args_pattern or args_enum")
	}
	// Free text (args_pattern) never becomes an argv element: only the
	// owner-written values of args_enum do. Free text goes through env.
	if argvArgs && len(f.ArgsEnum) == 0 {
		return fmt.Errorf("{args} as an argv element needs args_enum; pass free text in an %s* env variable", shellEnvPrefix)
	}
	if f.ArgsPattern != "" {
		// Compile the pattern on its own first: one with unbalanced groups,
		// like "a)|(.*", would otherwise close the wrapper's group and
		// escape the anchors.
		if _, errRaw := regexp.Compile(f.ArgsPattern); errRaw != nil {
			return fmt.Errorf("args_pattern: %w", errRaw)
		}
		re, errCompile := regexp.Compile(`^(?:` + f.ArgsPattern + `)$`)
		if errCompile != nil {
			return fmt.Errorf("args_pattern: %w", errCompile)
		}
		spec.argsPattern = re
	}
	spec.argsEnum = append([]string(nil), f.ArgsEnum...)
	return nil
}

// unsafeShellArgv refuses an argv that could hand {args} or {out} to an
// interpreter. An interpreter picks its script from its arguments
// (powershell's first bare argument is -Command; sh, python or node run the
// file their first argument names; python -m names a module), so once an
// interpreter or wrapper appears, as the program or later, no placeholder
// may follow it: env (AGENTBUS_*) is the only way in. A script file as the
// program (.bat, .ps1, .js, ...) is refused outright, since Windows runs it
// through an interpreter. Names are compared as Windows resolves them (see
// programFile and programBase). The mod repeats this check at run time.
func unsafeShellArgv(argv []string) error {
	if ext := scriptExtension(programFile(argv[0])); ext != "" {
		return fmt.Errorf("a %s program runs through an interpreter; call the interpreter with the script instead", ext)
	}
	interpreter := ""
	for _, el := range argv {
		if el == argsPlaceholder || el == outPlaceholder {
			if interpreter != "" {
				return fmt.Errorf("%s can't go to %s as an argument; pass it in an %s* env variable", el, interpreter, shellEnvPrefix)
			}
			continue
		}
		if interpreter == "" && isInterpreter(el) {
			interpreter = programFile(el)
		}
	}
	return nil
}

// isInterpreter reports whether el names an interpreter or wrapper, by its
// file name (rundll32) or without a version (tclsh8.6 is tclsh).
func isInterpreter(el string) bool {
	return shellInterpreters[programFile(el)] || shellInterpreters[programBase(el)]
}

// shellInterpreters are programs that run code, or another program, from
// their arguments, by programBase. The list can't be complete; the primary
// defence is that free text never becomes an argv element (args_enum only).
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true,
	"rbash": true, "ash": true, "mksh": true, "yash": true,
	"cmd": true, "powershell": true, "pwsh": true, "powershell_ise": true, "pwsh-preview": true,
	"python": true, "python2": true, "python3": true, "py": true, "pythonw": true, "pypy": true, "pypy3": true,
	"node": true, "nodejs": true, "deno": true, "bun": true, "perl": true, "ruby": true, "php": true, "lua": true, "tclsh": true,
	"r": true, "rscript": true,
	"osascript": true, "wscript": true, "cscript": true, "mshta": true,
	"awk": true, "gawk": true, "mawk": true, "sed": true,
	"ssh": true, "wsl": true, "ubuntu": true, "debian": true, "env": true, "xargs": true, "busybox": true,
	"sudo": true, "su": true, "doas": true, "runas": true, "watch": true, "script": true, "flock": true,
	"nice": true, "nohup": true, "timeout": true, "stdbuf": true, "time": true, "chroot": true, "setsid": true, "unbuffer": true,
	"conhost": true, "forfiles": true, "rundll32": true, "regsvr32": true, "wt": true,
}

// scriptExtensions are program files Windows hands to an interpreter.
var scriptExtensions = []string{".bat", ".cmd", ".ps1", ".vbs", ".js", ".wsf", ".hta"}

// programVersion is a version run right after the letters of a name, as in
// python3.12, tclsh8.6 or perl5.36.
var programVersion = regexp.MustCompile(`([a-z])[.0-9]*[0-9]$`)

// programFile is el's file name as Windows runs it: the base name, trailing
// dots and spaces dropped (Win32 ignores them), lowercased, and any .exe or
// .com suffixes removed (python.com.exe is python).
func programFile(el string) string {
	base := strings.ToLower(strings.TrimRight(el[strings.LastIndexAny(el, `/\`)+1:], ". "))
	for strings.HasSuffix(base, ".exe") || strings.HasSuffix(base, ".com") {
		base = strings.TrimRight(base[:len(base)-len(".exe")], ". ")
	}
	return base
}

// programBase is programFile without a -preview suffix or a version, the
// name an interpreter goes by.
func programBase(el string) string {
	base := strings.TrimSuffix(programFile(el), "-preview")
	return programVersion.ReplaceAllString(base, "$1")
}

// scriptExtension returns file's script extension, or "".
func scriptExtension(file string) string {
	for _, ext := range scriptExtensions {
		if strings.HasSuffix(file, ext) {
			return ext
		}
	}
	return ""
}

// registryEntry is one registry file: a spec, or the error that disabled it.
type registryEntry struct {
	name string
	spec *commandSpec
	err  error
}

// registry is the directory of command files. It re-reads the directory on
// lookup only when its listing or a file's size or mtime changed.
type registry struct {
	dir     string
	mu      sync.Mutex
	stamp   string
	loaded  bool
	entries map[string]registryEntry
	// warned maps a file name to the stamp last warned about, so a broken
	// file is logged once per change.
	warned  map[string]string
	dirWarn string
	// readDir and stat are os.ReadDir and os.Stat (tests replace them).
	readDir func(string) ([]os.DirEntry, error)
	stat    func(string) (os.FileInfo, error)
}

// newRegistry returns the registry in dir, or nil when dir is empty.
func newRegistry(dir string) *registry {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &registry{dir: dir, warned: map[string]string{}, readDir: os.ReadDir, stat: os.Stat}
}

// lookup returns the entry for name; a nil registry has none.
func (r *registry) lookup(name string) (registryEntry, bool) {
	if r == nil {
		return registryEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	e, ok := r.entries[name]
	return e, ok
}

// list returns every entry, sorted by name.
func (r *registry) list() []registryEntry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	out := make([]registryEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// commandFileStamp is one .yaml entry: its name, a stamp that changes when
// it does, and, when stat failed or it isn't a regular file, why it can't
// be loaded.
type commandFileStamp struct {
	file, stamp string
	err         error
}

func (r *registry) refreshLocked() {
	files, errDir := r.scanLocked()
	if errDir != nil {
		if errors.Is(errDir, fs.ErrNotExist) {
			// No directory, no commands.
			r.entries, r.stamp, r.loaded, r.dirWarn = nil, "", false, ""
			return
		}
		// Any other read error keeps the last good entries, so a passing
		// glitch can't turn registry commands into harness passthroughs.
		if msg := errDir.Error(); msg != r.dirWarn {
			r.dirWarn = msg
			log.Warnf("slack: read agent commands (keeping the last good list): %v", errDir)
		}
		return
	}
	r.dirWarn = ""
	var all strings.Builder
	for _, f := range files {
		all.WriteString(f.stamp + "\n")
	}
	if r.loaded && all.String() == r.stamp {
		return
	}
	entries := make(map[string]registryEntry, len(files))
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f.file] = true
		name := strings.TrimSuffix(f.file, commandFileExt)
		errLoad := f.err
		var spec *commandSpec
		if errLoad == nil {
			spec, errLoad = r.loadFile(f.file, name)
		}
		if errLoad == nil {
			entries[name] = registryEntry{name: name, spec: spec}
			delete(r.warned, f.file)
			continue
		}
		if r.warned[f.file] != f.stamp {
			r.warned[f.file] = f.stamp
			log.Warnf("slack: agent command file %s disabled: %v", f.file, errLoad)
		}
		if commandFileName.MatchString(name) && name != listCommandsName {
			entries[name] = registryEntry{name: name, err: errLoad}
		}
	}
	for file := range r.warned {
		if !present[file] {
			delete(r.warned, file)
		}
	}
	r.entries, r.stamp, r.loaded = entries, all.String(), true
}

// scanLocked lists the registry's .yaml entries with a stamp each (name,
// size, mtime), sorted by name. An entry that can't be stat'ed or isn't a
// regular file is listed with its error, so it shows up as misconfigured.
func (r *registry) scanLocked() ([]commandFileStamp, error) {
	dirEntries, errRead := r.readDir(r.dir)
	if errRead != nil {
		return nil, errRead
	}
	var out []commandFileStamp
	for _, de := range dirEntries {
		file := de.Name()
		if !strings.HasSuffix(file, commandFileExt) {
			continue
		}
		info, errStat := r.stat(filepath.Join(r.dir, file))
		switch {
		case errStat != nil:
			out = append(out, commandFileStamp{file: file, stamp: file + "\x00stat\x00" + errStat.Error(), err: errStat})
		case !info.Mode().IsRegular():
			out = append(out, commandFileStamp{file: file, stamp: file + "\x00mode\x00" + info.Mode().String(), err: errors.New("not a regular file")})
		default:
			out = append(out, commandFileStamp{
				file:  file,
				stamp: fmt.Sprintf("%s\x00%d\x00%d", file, info.Size(), info.ModTime().UnixNano()),
			})
		}
	}
	return out, nil
}

func (r *registry) loadFile(file, name string) (*commandSpec, error) {
	if !commandFileName.MatchString(name) {
		return nil, errors.New("the name must match [a-z0-9][a-z0-9_-]{0,31}")
	}
	if name == listCommandsName {
		return nil, errors.New("!commands is built in")
	}
	fh, errOpen := os.Open(filepath.Join(r.dir, file))
	if errOpen != nil {
		return nil, errOpen
	}
	data, errRead := io.ReadAll(io.LimitReader(fh, maxCommandFileSize+1))
	if errClose := fh.Close(); errClose != nil && errRead == nil {
		errRead = errClose
	}
	if errRead != nil {
		return nil, errRead
	}
	if len(data) > maxCommandFileSize {
		return nil, fmt.Errorf("file is over %d KiB", maxCommandFileSize>>10)
	}
	return parseCommandFile(name, data)
}

var (
	errNotACommand = errors.New("That isn't a command: write `!name args` (`!commands` lists them).")
	errArgsTooLong = fmt.Errorf("Command arguments are limited to %d characters.", maxCommandArgs)
)

// parseBang splits "!name rest": name is lowercased and checked, rest is
// trimmed and capped. An error's text is the reply to the owner.
func parseBang(text string) (name, rest string, err error) {
	t, ok := strings.CutPrefix(strings.TrimSpace(text), "!")
	if !ok {
		return "", "", errNotACommand
	}
	word, after := t, ""
	if i := strings.IndexFunc(t, unicode.IsSpace); i >= 0 {
		word, after = t[:i], t[i:]
	}
	name = strings.ToLower(word)
	if !commandName.MatchString(name) {
		return "", "", errNotACommand
	}
	rest = strings.TrimSpace(after)
	if utf8.RuneCountInString(rest) > maxCommandArgs {
		return "", "", errArgsTooLong
	}
	return name, rest, nil
}

// isBang reports whether text is written as a command.
func isBang(text string) bool { return strings.HasPrefix(strings.TrimSpace(text), "!") }

// commandList is the reply to !commands.
func (b *Bridge) commandList() string {
	var lines []string
	for _, e := range b.cmdRegistry.list() {
		if e.err != nil {
			lines = append(lines, fmt.Sprintf("• `!%s`: misconfigured", e.name))
			continue
		}
		line := fmt.Sprintf("• `!%s` (%s)", e.name, e.spec.Kind)
		if desc := strings.Join(strings.Fields(e.spec.Description), " "); desc != "" {
			line += ": " + escape(desc)
		}
		lines = append(lines, line)
	}
	head := "Registry commands:"
	if len(lines) == 0 {
		head = "No registry commands are set up."
	}
	return head + "\n" + strings.Join(append(lines,
		"Any Claude Code `/command` also works: write it as `!command` (for example `!compact`). Only owners can run commands."), "\n")
}
