package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
)

func TestClaudeCodeSettingsEndpoint(t *testing.T) {
	server := newTestServer(t)

	get := func(auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/claude-code/settings", nil)
		if auth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
		return w
	}

	if w := get(false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}

	w := get(true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"version":1,"models":[]}` {
		t.Fatalf("empty body = %s", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	updated := *server.cfg
	updated.SDKConfig = server.cfg.SDKConfig
	updated.ClaudeCode.GatewayModels = []proxyconfig.ClaudeGatewayModel{
		{ID: "gpt-6.1-sol", Label: "GPT 6.1 Sol", BehavesAs: "claude-fable-5-1", AutoCompactWindow: 500000, EffortLevel: "xhigh"},
		{ID: "bare-model", BehavesAs: "claude-opus-5"},
	}
	updated.ProxyURL = "http://user:secret@proxy.invalid"
	server.UpdateClients(&updated)

	w = get(true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	want := `{"version":1,"models":[{"model":"claude-gw-gpt-6.1-sol","label":"GPT 6.1 Sol","behavesAs":"claude-fable-5-1","autoCompactWindow":500000,"effortLevel":"xhigh"},{"model":"claude-gw-bare-model","behavesAs":"claude-opus-5"}]}`
	if w.Body.String() != want {
		t.Fatalf("body = %s\nwant   %s", w.Body.String(), want)
	}
	for _, secret := range []string{"secret", "test-key", "proxy"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("response leaks %q: %s", secret, w.Body.String())
		}
	}
	// DTO whitelist: only version and models at top level; fixed key set per model.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil || len(top) != 2 {
		t.Fatalf("unexpected top-level keys: %v %v", top, err)
	}
	var models []map[string]any
	if err := json.Unmarshal(top["models"], &models); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"model": true, "label": true, "behavesAs": true, "autoCompactWindow": true, "effortLevel": true}
	for _, m := range models {
		for k := range m {
			if !allowed[k] {
				t.Fatalf("unexpected key %q", k)
			}
		}
	}
}

func TestGatewayModelsDerivedHiddenLocalListing(t *testing.T) {
	registryRef := registry.GetGlobalRegistry()
	const clientID = "test-gateway-derived-hidden"
	registryRef.RegisterClient(clientID, "claude", []*registry.ModelInfo{
		{ID: "gwtest-default", Object: "model", OwnedBy: "test"},
		{ID: "gwtest-true", Object: "model", OwnedBy: "test"},
		{ID: "gwtest-false", Object: "model", OwnedBy: "test"},
		{ID: "gwtest-manual", Object: "model", OwnedBy: "test"},
		{ID: "gwtest-plain", Object: "model", OwnedBy: "test"},
	})
	t.Cleanup(func() { registryRef.UnregisterClient(clientID) })

	server := newTestServer(t)
	tr, fl := true, false
	updated := *server.cfg
	updated.SDKConfig = server.cfg.SDKConfig
	updated.ClaudeCode.HiddenModels = []string{"gwtest-manual"}
	updated.ClaudeCode.GatewayModels = []proxyconfig.ClaudeGatewayModel{
		{ID: "gwtest-default", BehavesAs: "claude-opus-5"},
		{ID: "gwtest-true", BehavesAs: "claude-opus-5", HideFromDiscovery: &tr},
		{ID: "gwtest-false", BehavesAs: "claude-opus-5", HideFromDiscovery: &fl},
	}
	server.UpdateClients(&updated)

	list := func(anthropic bool) string {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		if anthropic {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	body := list(true)
	has := func(id string) bool { return gjson.Get(body, `data.#(id=="claude-gw-`+id+`")`).Exists() }
	if has("gwtest-default") || has("gwtest-true") || has("gwtest-manual") {
		t.Fatalf("hidden models still listed: %s", body)
	}
	if !has("gwtest-false") || !has("gwtest-plain") {
		t.Fatalf("visible models missing: %s", body)
	}
	if other := list(false); !strings.Contains(other, "gwtest-default") || !strings.Contains(other, "gwtest-true") {
		t.Fatalf("non-Anthropic listing must keep gateway models: %s", other)
	}
	// The raw config is not mutated by derivation.
	if len(server.cfg.ClaudeCode.HiddenModels) != 1 {
		t.Fatalf("HiddenModels mutated: %v", server.cfg.ClaudeCode.HiddenModels)
	}
	// Removing the rows stops the derived hiding.
	cleared := updated
	cleared.ClaudeCode.GatewayModels = nil
	server.UpdateClients(&cleared)
	body = list(true)
	if !has("gwtest-default") || !has("gwtest-true") || has("gwtest-manual") {
		t.Fatalf("after removing rows: %s", body)
	}
}

func TestGatewayModelsDerivedHiddenHomeListing(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Home.Enabled = true
	tr, fl := true, false
	server.cfg.ClaudeCode.HiddenModels = []string{"gwhome-manual"}
	server.cfg.ClaudeCode.GatewayModels = []proxyconfig.ClaudeGatewayModel{
		{ID: "gwhome-default", BehavesAs: "claude-opus-5"},
		{ID: "gwhome-true", BehavesAs: "claude-opus-5", HideFromDiscovery: &tr},
		{ID: "gwhome-false", BehavesAs: "claude-opus-5", HideFromDiscovery: &fl},
	}
	engine := gin.New()
	engine.GET("/v1/models", server.unifiedModelsHandler(nil, nil))
	client := newHomeCatalogClient(t, `{"claude":[{"id":"gwhome-default"},{"id":"gwhome-true"},{"id":"gwhome-false"},{"id":"gwhome-manual"},{"id":"gwhome-plain"}]}`)
	previousHome := home.Current()
	home.SetCurrent(client)
	t.Cleanup(func() { home.SetCurrent(previousHome) })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	has := func(id string) bool { return gjson.Get(body, `data.#(id=="claude-gw-`+id+`")`).Exists() }
	if has("gwhome-default") || has("gwhome-true") || has("gwhome-manual") {
		t.Fatalf("hidden models still listed: %s", body)
	}
	if !has("gwhome-false") || !has("gwhome-plain") {
		t.Fatalf("visible models missing: %s", body)
	}
}
