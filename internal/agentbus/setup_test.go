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
