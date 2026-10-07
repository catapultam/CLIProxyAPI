package marketplace

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBaseURLPrefersForwardedHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace.json", nil)
	req.Host = "127.0.0.1:8317"
	req.Header.Set("X-Forwarded-Host", "proxy.tailnet:8317, other.example")
	if got, want := BaseURL(req), "https://proxy.tailnet:8317"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
}

func TestBaseURLPrefersPublicURLEnv(t *testing.T) {
	t.Setenv(publicURLEnv, " https://cakebox.example.ts.net:8444/ ")
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Host = "cakebox.example.ts.net:8317"
	req.Header.Set("X-Forwarded-Host", "proxy.tailnet:8317")
	if got, want := BaseURL(req), "https://cakebox.example.ts.net:8444"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
}

func TestBaseURLFallsBackToHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace.json", nil)
	req.Host = "cakebox:8317"
	if got, want := BaseURL(req), "https://cakebox:8317"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
}

func TestBuildDocShape(t *testing.T) {
	doc, err := BuildDoc("https://cakebox:8317")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "homelab" || doc.Owner.Name != "catapultam" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(doc.Plugins) != 2 || doc.Plugins[1].Name != "denial-prompt" {
		t.Fatalf("plugins = %+v", doc.Plugins)
	}
	p := doc.Plugins[0]
	if p.Name != "agentbus" || p.Description == "" {
		t.Fatalf("plugin = %+v", p)
	}
	if p.Source.Source != "archive" {
		t.Fatalf("source.source = %q", p.Source.Source)
	}
	wantURL := "https://cakebox:8317/plugins/agentbus-0.4.1.zip"
	if p.Source.URL != wantURL {
		t.Fatalf("source.url = %q, want %q", p.Source.URL, wantURL)
	}
	asset, ok, errGet := Get("agentbus")
	if errGet != nil || !ok {
		t.Fatalf("Get(agentbus) = %v, %v, %v", asset, ok, errGet)
	}
	if p.Source.SHA256 != asset.SHA256 {
		t.Fatalf("doc sha256 %q != asset sha256 %q", p.Source.SHA256, asset.SHA256)
	}
	if len(p.Source.SHA256) != 64 {
		t.Fatalf("sha256 length = %d, want 64", len(p.Source.SHA256))
	}
}
