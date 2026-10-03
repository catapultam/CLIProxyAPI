package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestEffectiveSDKConfigCopiesModelRewrite(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.ModelRewrite = []config.ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}}
	sdkCfg := effectiveSDKConfig(cfg)
	if sdkCfg == nil || !reflect.DeepEqual(sdkCfg.ModelRewrite, cfg.Routing.ModelRewrite) {
		t.Fatalf("SDK ModelRewrite = %#v, want %#v", sdkCfg.ModelRewrite, cfg.Routing.ModelRewrite)
	}
	cfg.Routing.ModelRewrite[0].To = "changed"
	if sdkCfg.ModelRewrite[0].To != "claude-sonnet-5-5" {
		t.Fatal("effective SDK config shares model rewrite state")
	}
}

// Reloading the config applies new rules to handlers, and GET /v1/usage reports
// the pool of the rewritten model.
func TestModelRewriteHotReloadAndUsageQuery(t *testing.T) {
	server := newTestServer(t)
	usageModel := func(query string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/usage?model="+query, nil)
		request.Header.Set("Authorization", "Bearer test-key")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/usage status = %d, body %s", recorder.Code, recorder.Body.String())
		}
		return gjson.Get(recorder.Body.String(), "model").String()
	}
	if got := usageModel("claude-sonnet-4-5"); got != "claude-sonnet-4-5" {
		t.Fatalf("usage model without rules = %q, want claude-sonnet-4-5", got)
	}

	parsed, errParse := config.ParseConfigBytes([]byte("routing:\n  model-rewrite:\n    - match: '*sonnet*'\n      to: claude-sonnet-5-5\n    - match: ' '\n      to: dropped\n"))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	updated := *server.cfg
	updated.Routing = parsed.Routing
	server.UpdateClients(&updated)
	if got := server.handlers.RewriteModelName("anthropic/claude-sonnet-4"); got != "claude-sonnet-5-5" {
		t.Fatalf("reloaded handler rewrite = %q, want claude-sonnet-5-5", got)
	}
	if got := usageModel("claude-sonnet-4-5"); got != "claude-sonnet-5-5" {
		t.Fatalf("usage model with rules = %q, want claude-sonnet-5-5", got)
	}
	if got := usageModel("gpt-5.5"); got != "gpt-5.5" {
		t.Fatalf("usage model without a matching rule = %q, want gpt-5.5", got)
	}

	cleared := *server.cfg
	cleared.Routing.ModelRewrite = nil
	server.UpdateClients(&cleared)
	if got := usageModel("claude-sonnet-4-5"); got != "claude-sonnet-4-5" {
		t.Fatalf("usage model after removing rules = %q, want claude-sonnet-4-5", got)
	}
}
