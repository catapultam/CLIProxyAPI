package slackbridge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
)

// commandFile is one <name>.yaml in the registry. Unknown keys are rejected.
type commandFile struct {
	Description    string              `yaml:"description"`
	Kind           string              `yaml:"kind"`
	Command        string              `yaml:"command"`
	Args           string              `yaml:"args"`
	Text           string              `yaml:"text"`
	Argv           map[string][]string `yaml:"argv"`
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
	Output      string
	Timeout     int
	// argsPattern (anchored) or argsEnum limits a shell command's
	// arguments. With neither, a shell command takes no arguments.
	argsPattern *regexp.Regexp
	argsEnum    []string
}

// checkArgs reports why rest isn't acceptable for c, as a reply to the
// owner. Slash and prompt commands take free-form arguments.
func (c *commandSpec) checkArgs(rest string) error {
	if c.Kind != agentbus.CommandShell {
		return nil
	}
	invalid := fmt.Errorf("invalid arguments for `!%s`.", c.Name)
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
		return fmt.Errorf("`!%s` takes no arguments.", c.Name)
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
		"argv": f.Argv != nil, "output": f.Output != "", "timeout_seconds": f.TimeoutSeconds != 0,
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
				continue
			}
			if strings.Contains(el, argsPlaceholder) || strings.Contains(el, outPlaceholder) {
				return fmt.Errorf("argv.%s: {args} and {out} must be whole argv elements", osKey)
			}
		}
		if spec.Output == "image" && !slices.Contains(argv, outPlaceholder) {
			return fmt.Errorf("argv.%s: output image needs an {out} element", osKey)
		}
		spec.Argv[osKey] = append([]string(nil), argv...)
	}
	if f.ArgsPattern != "" && len(f.ArgsEnum) > 0 {
		return errors.New("declare args_pattern or args_enum, not both")
	}
	if f.ArgsPattern != "" {
		re, errCompile := regexp.Compile(`^(?:` + f.ArgsPattern + `)$`)
		if errCompile != nil {
			return fmt.Errorf("args_pattern: %w", errCompile)
		}
		spec.argsPattern = re
	}
	spec.argsEnum = append([]string(nil), f.ArgsEnum...)
	return nil
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
}

// newRegistry returns the registry in dir, or nil when dir is empty.
func newRegistry(dir string) *registry {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &registry{dir: dir, warned: map[string]string{}}
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

type commandFileStamp struct {
	file, stamp string
}

func (r *registry) refreshLocked() {
	files, errDir := r.scanLocked()
	if errDir != nil {
		r.entries, r.stamp, r.loaded = nil, "", false
		if errors.Is(errDir, fs.ErrNotExist) {
			r.dirWarn = ""
			return
		}
		if msg := errDir.Error(); msg != r.dirWarn {
			r.dirWarn = msg
			log.Warnf("slack: read agent commands: %v", errDir)
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
		spec, errLoad := r.loadFile(f.file, name)
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

// scanLocked lists the registry's .yaml files with a stamp each (name, size,
// mtime), sorted by name.
func (r *registry) scanLocked() ([]commandFileStamp, error) {
	dirEntries, errRead := os.ReadDir(r.dir)
	if errRead != nil {
		return nil, errRead
	}
	var out []commandFileStamp
	for _, de := range dirEntries {
		if !strings.HasSuffix(de.Name(), commandFileExt) {
			continue
		}
		info, errStat := os.Stat(filepath.Join(r.dir, de.Name()))
		if errStat != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, commandFileStamp{
			file:  de.Name(),
			stamp: fmt.Sprintf("%s\x00%d\x00%d", de.Name(), info.Size(), info.ModTime().UnixNano()),
		})
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
