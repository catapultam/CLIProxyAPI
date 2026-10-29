package management

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const testEmbedSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeEmbedSecret(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "embed_secret")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func embedEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestMetricsEmbedSignsFixedDashboard(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	env := embedEnv(map[string]string{
		metricsEmbedURLEnv:        "https://cakebox.example.ts.net:8447/",
		metricsEmbedDashboardEnv:  "7",
		metricsEmbedSecretFileEnv: writeEmbedSecret(t, testEmbedSecret),
	})
	embedURL, expiresAt, err := metricsEmbedFromEnv(env, now)
	if err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("expires_at = %v", expiresAt)
	}
	prefix := "https://cakebox.example.ts.net:8447/embed/dashboard/"
	suffix := "#bordered=true&titled=true"
	if !strings.HasPrefix(embedURL, prefix) || !strings.HasSuffix(embedURL, suffix) {
		t.Fatalf("url = %s", embedURL)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(embedURL, prefix), suffix)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token parts = %d", len(parts))
	}
	mac := hmac.New(sha256.New, []byte(testEmbedSecret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		t.Fatal("bad HS256 signature")
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(header) != `{"alg":"HS256","typ":"JWT"}` {
		t.Fatalf("header = %s", header)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"resource": map[string]any{"dashboard": float64(7)}, "params": map[string]any{}, "exp": float64(now.Unix() + 600)}
	gotJSON, _ := json.Marshal(claims)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("claims = %s want %s", gotJSON, wantJSON)
	}
}

func TestMetricsEmbedRejectsInvalidConfig(t *testing.T) {
	secretFile := writeEmbedSecret(t, testEmbedSecret)
	if _, _, err := metricsEmbedFromEnv(embedEnv(nil), time.Now()); err != errMetricsEmbedNotConfigured {
		t.Fatalf("empty env: %v", err)
	}
	for name, env := range map[string]map[string]string{
		"http url":     {metricsEmbedURLEnv: "http://x.example", metricsEmbedDashboardEnv: "1", metricsEmbedSecretFileEnv: secretFile},
		"url query":    {metricsEmbedURLEnv: "https://x.example/?a=1", metricsEmbedDashboardEnv: "1", metricsEmbedSecretFileEnv: secretFile},
		"url userinfo": {metricsEmbedURLEnv: "https://u:p@x.example", metricsEmbedDashboardEnv: "1", metricsEmbedSecretFileEnv: secretFile},
		"zero id":      {metricsEmbedURLEnv: "https://x.example", metricsEmbedDashboardEnv: "0", metricsEmbedSecretFileEnv: secretFile},
		"text id":      {metricsEmbedURLEnv: "https://x.example", metricsEmbedDashboardEnv: "7a", metricsEmbedSecretFileEnv: secretFile},
		"no secret":    {metricsEmbedURLEnv: "https://x.example", metricsEmbedDashboardEnv: "1"},
		"missing file": {metricsEmbedURLEnv: "https://x.example", metricsEmbedDashboardEnv: "1", metricsEmbedSecretFileEnv: secretFile + ".none"},
		"short secret": {metricsEmbedURLEnv: "https://x.example", metricsEmbedDashboardEnv: "1", metricsEmbedSecretFileEnv: writeEmbedSecret(t, "short")},
	} {
		if _, _, err := metricsEmbedFromEnv(embedEnv(env), time.Now()); err == nil || err == errMetricsEmbedNotConfigured {
			t.Errorf("%s: err = %v", name, err)
		} else if strings.Contains(err.Error(), testEmbedSecret) {
			t.Errorf("%s: error exposes secret", name)
		}
	}
}

func TestGetMetricsEmbedResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	call := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		(&Handler{}).GetMetricsEmbed(c)
		return recorder
	}
	for _, key := range []string{metricsEmbedURLEnv, metricsEmbedDashboardEnv, metricsEmbedSecretFileEnv} {
		t.Setenv(key, "")
	}
	if r := call(); r.Code != http.StatusOK || strings.TrimSpace(r.Body.String()) != `{"configured":false}` ||
		r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("not configured: %d %s %q", r.Code, r.Body.String(), r.Header().Get("Cache-Control"))
	}

	t.Setenv(metricsEmbedURLEnv, "https://x.example")
	t.Setenv(metricsEmbedDashboardEnv, "3")
	t.Setenv(metricsEmbedSecretFileEnv, writeEmbedSecret(t, testEmbedSecret))
	r := call()
	var body struct {
		Configured bool   `json:"configured"`
		URL        string `json:"url"`
		ExpiresAt  string `json:"expires_at"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil || r.Code != http.StatusOK || !body.Configured ||
		!strings.HasPrefix(body.URL, "https://x.example/embed/dashboard/") || body.ExpiresAt == "" {
		t.Fatalf("configured: %d %s", r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), testEmbedSecret) || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret exposed or cache header missing")
	}

	t.Setenv(metricsEmbedDashboardEnv, "-1")
	if r := call(); r.Code != http.StatusInternalServerError || strings.Contains(r.Body.String(), testEmbedSecret) {
		t.Fatalf("invalid: %d %s", r.Code, r.Body.String())
	}
}
