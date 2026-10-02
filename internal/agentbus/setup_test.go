package agentbus

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

func scriptServer(t *testing.T) (*Store, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := NewStore("", nil)
	s.waitTimeout = 2 * time.Second
	r := gin.New()
	s.Register(r.Group("/v1/agentbus"))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return s, srv
}

func scriptEnv(home, baseURL string) []string {
	return []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"ANTHROPIC_BASE_URL=" + baseURL,
		"ANTHROPIC_AUTH_TOKEN=test-token",
		"TMPDIR=" + home,
	}
}

func TestSetupScriptInstallsHooksIdempotently(t *testing.T) {
	requireTools(t, "sh", "python3", "curl")
	_, srv := scriptServer(t)
	home := t.TempDir()
	configDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{"env":{"A":"1"},"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"/x/peon.sh","timeout":10}]}]}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cmd := exec.Command("sh", "-c", setupScript)
		cmd.Env = scriptEnv(home, srv.URL)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup run %d: %v\n%s", i, err, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env   map[string]string `json:"env"`
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err = json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	waiter := filepath.Join(configDir, "hooks", "agentbus", "wait.sh")
	count := func(event, command string) int {
		n := 0
		for _, g := range settings.Hooks[event] {
			for _, h := range g.Hooks {
				if h["command"] == command {
					n++
				}
			}
		}
		return n
	}
	if count("SessionStart", "/x/peon.sh") != 1 || settings.Env["A"] != "1" {
		t.Fatalf("existing settings not preserved: %s", raw)
	}
	if count("SessionStart", waiter) != 1 || count("Stop", waiter) != 1 {
		t.Fatalf("waiter hooks not installed exactly once: %s", raw)
	}
	info, err := os.Stat(waiter)
	if err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("waiter not installed executable: %v", err)
	}
	if _, err = os.Stat(filepath.Join(configDir, "settings.json.bak-agentbus")); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func runWaiter(t *testing.T, baseURL, stdin string) (int, string) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command("sh", "-c", waiterScript)
	cmd.Env = scriptEnv(home, baseURL)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), stderr.String()
	default:
		t.Fatalf("run waiter: %v", err)
		return -1, ""
	}
}

func TestWaiterWakesWithMessageAndAppliesRename(t *testing.T) {
	requireTools(t, "sh", "python3", "curl")
	s, srv := scriptServer(t)
	s.Hello(sidB, "pc", "/b", "")
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"user"}` + "\n" + `{"type":"custom-title","customTitle":"old-name","sessionId":"x"}` + "\n" + `{"type":"custom-title","customTitle":"ci-runner","sessionId":"x"}` + "\n"
	if err := os.WriteFile(transcript, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		for s.Address(sidA) == "" {
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = s.Send(sidB, s.Address(sidA), "run the nightly build", "")
	}()
	stdin := `{"session_id":"` + sidA + `","cwd":"/home/a/proj","transcript_path":"` + transcript + `"}`
	code, stderr := runWaiter(t, srv.URL, stdin)
	if code != 2 || !strings.Contains(stderr, "run the nightly build") || !strings.Contains(stderr, `"from_session":"`+sidA+`"`) {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if id, ok := s.Resolve("ci-runner"); !ok || id != sidA {
		t.Fatalf("rename not applied: %q %v", id, ok)
	}
}

func TestWaiterExitsQuietlyWhenSuperseded(t *testing.T) {
	requireTools(t, "sh", "python3", "curl")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	code, stderr := runWaiter(t, srv.URL, `{"session_id":"`+sidA+`","cwd":"/a"}`)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestWaiterExitsWithoutConfiguration(t *testing.T) {
	requireTools(t, "sh")
	cmd := exec.Command("sh", "-c", waiterScript)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(`{"session_id":"x"}`)
	if err := cmd.Run(); err != nil {
		t.Fatalf("waiter without config: %v", err)
	}
}

// brokenPython3Path returns a PATH whose first entry holds a python3 that
// fails like the Windows Store stub, and a working python next to it.
func brokenPython3Path(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	stub := "#!/bin/sh\necho 'Python was not found; run without arguments to install from the Microsoft Store' >&2\nexit 9\n"
	if err = os.WriteFile(filepath.Join(dir, "python3"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(real, filepath.Join(dir, "python")); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestWaiterSkipsBrokenPython3(t *testing.T) {
	requireTools(t, "sh", "curl")
	s, srv := scriptServer(t)
	s.Hello(sidB, "pc", "/b", "")
	go func() {
		for s.Address(sidA) == "" {
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = s.Send(sidB, s.Address(sidA), "stub-proof", "")
	}()
	home := t.TempDir()
	cmd := exec.Command("sh", "-c", waiterScript)
	cmd.Env = append(scriptEnv(home, srv.URL), "PATH="+brokenPython3Path(t))
	cmd.Stdin = strings.NewReader(`{"session_id":"` + sidA + `","cwd":"/a"}`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(stderr.String(), "stub-proof") {
		t.Fatalf("err %v, stderr:\n%s", err, stderr.String())
	}
}

func TestWaiterCheckReportsProblems(t *testing.T) {
	requireTools(t, "sh", "curl", "python3")
	_, srv := scriptServer(t)
	home := t.TempDir()

	ok := exec.Command("sh", "-c", waiterScript, "wait.sh", "--check")
	ok.Env = scriptEnv(home, srv.URL)
	if out, err := ok.CombinedOutput(); err != nil || !strings.Contains(string(out), "agentbus: OK") {
		t.Fatalf("healthy check: %v\n%s", err, out)
	}

	noPython := exec.Command("sh", "-c", waiterScript, "wait.sh", "--check")
	noPython.Env = append(scriptEnv(home, srv.URL), "PATH=/nonexistent:/bin:/usr/bin", "AGENTBUS_PYTHON=/nonexistent/python")
	stubDir := t.TempDir()
	stub := "#!/bin/sh\nexit 9\n"
	for _, name := range []string{"python3", "python", "py"} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	noPython.Env = append(scriptEnv(home, srv.URL), "PATH="+stubDir+":/bin:/usr/bin")
	out, err := noPython.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no working Python") {
		t.Fatalf("broken python check: %v\n%s", err, out)
	}

	unreachable := exec.Command("sh", "-c", waiterScript, "wait.sh", "--check")
	unreachable.Env = scriptEnv(home, "http://127.0.0.1:1")
	out, err = unreachable.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "cannot reach") {
		t.Fatalf("unreachable check: %v\n%s", err, out)
	}
}

func TestSetupFailsLoudlyWithoutWorkingPython(t *testing.T) {
	requireTools(t, "sh", "curl")
	_, srv := scriptServer(t)
	stubDir := t.TempDir()
	for _, name := range []string{"python3", "python", "py"} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", setupScript)
	cmd.Env = append(scriptEnv(t.TempDir(), srv.URL), "PATH="+stubDir+":/bin:/usr/bin")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no working Python") {
		t.Fatalf("setup with broken python: %v\n%s", err, out)
	}
}
