package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestMetricsEmbedRouteNeedsManagementAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = filepath.Dir(path)
	h := management.NewHandler(cfg, path, nil)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	t.Setenv("USAGE_ANALYTICS_METABASE_URL", "")
	t.Setenv("USAGE_ANALYTICS_METABASE_DASHBOARD_ID", "")
	t.Setenv("USAGE_ANALYTICS_EMBED_SECRET_FILE", "")
	for _, tc := range []struct {
		auth bool
		want int
	}{{false, http.StatusUnauthorized}, {true, http.StatusOK}} {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/metrics-embed", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if tc.auth {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		recorder := httptest.NewRecorder()
		s.engine.ServeHTTP(recorder, req)
		if recorder.Code != tc.want {
			t.Fatalf("auth=%v: status=%d body=%s", tc.auth, recorder.Code, recorder.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v0/management/observability/usage/metrics-embed", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer test-password")
	recorder := httptest.NewRecorder()
	s.engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("v0 route: status=%d", recorder.Code)
	}
}
