package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8GatewayModelsRoundTripAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	file := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver:\n  port: 8317\naccess:\n  api-keys: [client]\n"
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: file}
	router := gin.New()
	router.GET("/v8/management/config/*path", h.ConfigV8)
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	const url = "/v8/management/config/upstream/claude/gateway-models"

	do := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, url, strings.NewReader(body)))
		return w
	}

	good := `[{"id":"gpt-6.1-sol","label":"GPT 6.1 Sol","behaves-as":"claude-fable-5-1","auto-compact-window":500000,"effort-level":"xhigh","hide-from-discovery":false},{"id":"other-model","behaves-as":"claude-opus-5"}]`
	if w := do(http.MethodPut, good); w.Code != http.StatusOK {
		t.Fatalf("put: status=%d body=%s", w.Code, w.Body.String())
	}
	w := do(http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", w.Code, w.Body.String())
	}
	var rows []map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["id"] != "gpt-6.1-sol" || rows[0]["behaves-as"] != "claude-fable-5-1" ||
		rows[0]["auto-compact-window"] != float64(500000) || rows[0]["effort-level"] != "xhigh" || rows[0]["hide-from-discovery"] != false ||
		rows[1]["id"] != "other-model" {
		t.Fatalf("round trip mismatch: %s", w.Body.String())
	}
	loaded, err := config.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ClaudeCode.GatewayModels; len(got) != 2 || got[0].ID != "gpt-6.1-sol" || got[0].Hidden() || !got[1].Hidden() {
		t.Fatalf("runtime config mismatch: %+v", got)
	}
	saved, _ := os.ReadFile(file)

	for name, body := range map[string]string{
		"claude id":     `[{"id":"claude-x","behaves-as":"claude-opus-5"}]`,
		"duplicate":     `[{"id":"Gpt-1","behaves-as":"claude-opus-5"},{"id":"gpt-1","behaves-as":"claude-opus-5"}]`,
		"bad behaves":   `[{"id":"gpt-1","behaves-as":"gpt-2"}]`,
		"bad effort":    `[{"id":"gpt-1","behaves-as":"claude-opus-5","effort-level":"max"}]`,
		"bad window":    `[{"id":"gpt-1","behaves-as":"claude-opus-5","auto-compact-window":5}]`,
		"bad type":      `[{"id":"gpt-1","behaves-as":"claude-opus-5","auto-compact-window":"big"}]`,
		"bad hide type": `[{"id":"gpt-1","behaves-as":"claude-opus-5","hide-from-discovery":"maybe"}]`,
		"wildcard":      `[{"id":"gpt-*","behaves-as":"claude-opus-5"}]`,
	} {
		if w := do(http.MethodPut, body); w.Code < 400 {
			t.Fatalf("%s: status=%d, want rejection; body=%s", name, w.Code, w.Body.String())
		}
	}
	after, _ := os.ReadFile(file)
	if string(after) != string(saved) {
		t.Fatal("rejected writes changed the config file")
	}
}
