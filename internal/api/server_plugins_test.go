package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/marketplace"
)

func TestPluginsMarketplaceJSONNeedsNoAuth(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)

	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace.json", nil)
	req.Host = "cakebox:8317"
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}

	var doc marketplace.Doc
	if errUnmarshal := json.Unmarshal(w.Body.Bytes(), &doc); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v, body = %s", errUnmarshal, w.Body)
	}
	if doc.Name != "homelab" || doc.Owner.Name != "catapultam" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(doc.Plugins) != 2 || doc.Plugins[0].Name != "agentbus" || doc.Plugins[1].Name != "denial-prompt" {
		t.Fatalf("plugins = %+v", doc.Plugins)
	}
	wantURL := "https://cakebox:8317/plugins/agentbus-0.3.10.zip"
	if doc.Plugins[0].Source.URL != wantURL {
		t.Fatalf("url = %q, want %q", doc.Plugins[0].Source.URL, wantURL)
	}
	wantURL = "https://cakebox:8317/plugins/denial-prompt-0.1.0.zip"
	if doc.Plugins[1].Source.URL != wantURL {
		t.Fatalf("url = %q, want %q", doc.Plugins[1].Source.URL, wantURL)
	}
}

func TestPluginsMarketplaceJSONUsesForwardedHost(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)

	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace.json", nil)
	req.Host = "127.0.0.1:8317"
	req.Header.Set("X-Forwarded-Host", "proxy.tailnet:8317")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	var doc marketplace.Doc
	if errUnmarshal := json.Unmarshal(w.Body.Bytes(), &doc); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	wantURL := "https://proxy.tailnet:8317/plugins/agentbus-0.3.10.zip"
	if doc.Plugins[0].Source.URL != wantURL {
		t.Fatalf("url = %q, want %q", doc.Plugins[0].Source.URL, wantURL)
	}
}

func TestPluginsZipMatchesManifestSHA256(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)

	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace.json", nil)
	req.Host = "cakebox:8317"
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	var doc marketplace.Doc
	if errUnmarshal := json.Unmarshal(w.Body.Bytes(), &doc); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}

	zipReq := httptest.NewRequest(http.MethodGet, "/plugins/agentbus-0.3.10.zip", nil)
	zipW := httptest.NewRecorder()
	s.engine.ServeHTTP(zipW, zipReq)
	if zipW.Code != http.StatusOK {
		t.Fatalf("zip status = %d", zipW.Code)
	}
	if got := zipW.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q", got)
	}

	sum := sha256.Sum256(zipW.Body.Bytes())
	sumHex := hex.EncodeToString(sum[:])
	if sumHex != doc.Plugins[0].Source.SHA256 {
		t.Fatalf("served zip sha256 %q != manifest sha256 %q", sumHex, doc.Plugins[0].Source.SHA256)
	}
}

func TestPluginsUnknownZipNotFound(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)

	for _, name := range []string{"agentbus-9.9.9.zip", "nope-0.0.1.zip", "not-a-zip"} {
		req := httptest.NewRequest(http.MethodGet, "/plugins/"+name, nil)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET /plugins/%s = %d, want 404", name, w.Code)
		}
	}
}
