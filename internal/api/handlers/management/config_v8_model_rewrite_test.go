package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The WebUI reads routing.model-rewrite from GET /v8/management/config and saves
// it with PATCH {"routing":{"model-rewrite":[...]}}. The PATCH replaces the list,
// sanitizes it, persists it to config.yaml and hot-applies it.
func TestConfigV8PatchModelRewriteRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nrouting:\n  strategy: fill-first\n  model-rewrite:\n    - match: '*old*'\n      to: old-target\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan *config.Config, 4)
	h := &Handler{cfg: cfg, configFilePath: path}
	h.configReloadHook = func(_ context.Context, next *config.Config) { applied <- next }
	router := gin.New()
	router.GET("/v8/management/config", h.ConfigV8)
	router.PATCH("/v8/management/config", h.ConfigV8)

	readRules := func() []config.ModelRewriteRule {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET config: status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Routing struct {
				Strategy     string                    `json:"strategy"`
				ModelRewrite []config.ModelRewriteRule `json:"model-rewrite"`
			} `json:"routing"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
			t.Fatalf("decode GET config: %v", errDecode)
		}
		if body.Routing.Strategy != "fill-first" {
			t.Fatalf("routing.strategy = %q, want fill-first", body.Routing.Strategy)
		}
		return body.Routing.ModelRewrite
	}
	if got, want := readRules(), []config.ModelRewriteRule{{Match: "*old*", To: "old-target"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial rules = %#v, want %#v", got, want)
	}

	patch := `{"routing":{"model-rewrite":[{"match":" *sonnet* ","to":"claude-sonnet-5-5"},{"match":"","to":"dropped"},{"match":"*opus*","to":"claude-opus-5-5"}]}}`
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(patch)))
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH config: status=%d body=%s", response.Code, response.Body.String())
	}
	want := []config.ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}, {Match: "*opus*", To: "claude-opus-5-5"}}

	// Replaced and sanitized in the API view.
	if got := readRules(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rules after PATCH = %#v, want %#v", got, want)
	}
	// Persisted, sanitized, to config.yaml.
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "dropped") || strings.Contains(string(saved), "*old*") {
		t.Fatalf("saved config kept stale or invalid rules:\n%s", saved)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Routing.ModelRewrite, want) {
		t.Fatalf("loaded rules = %#v, want %#v\n%s", loaded.Routing.ModelRewrite, want, saved)
	}
	// Hot-applied through the reload hook.
	next := <-applied
	if !reflect.DeepEqual(next.Routing.ModelRewrite, want) {
		t.Fatalf("hot-applied rules = %#v, want %#v", next.Routing.ModelRewrite, want)
	}

	// An empty list clears the rules.
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(`{"routing":{"model-rewrite":[]}}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH clear: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := readRules(); len(got) != 0 {
		t.Fatalf("rules after clearing = %#v, want none", got)
	}
	if next = <-applied; len(next.Routing.ModelRewrite) != 0 {
		t.Fatalf("hot-applied rules after clearing = %#v, want none", next.Routing.ModelRewrite)
	}
}
