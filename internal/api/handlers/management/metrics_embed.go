package management

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Metabase static embedding of one fixed dashboard. Configuration is
// environment-only; the caller cannot choose the resource or parameters.
const (
	metricsEmbedURLEnv        = "USAGE_ANALYTICS_METABASE_URL"
	metricsEmbedDashboardEnv  = "USAGE_ANALYTICS_METABASE_DASHBOARD_ID"
	metricsEmbedSecretFileEnv = "USAGE_ANALYTICS_EMBED_SECRET_FILE"
	metricsEmbedLifetime      = 10 * time.Minute
	metricsEmbedMinSecretLen  = 32
)

var errMetricsEmbedNotConfigured = errors.New("metrics embed not configured")

// GetMetricsEmbed returns a short-lived signed Metabase embed URL.
func (h *Handler) GetMetricsEmbed(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	embedURL, expiresAt, err := metricsEmbedFromEnv(os.Getenv, time.Now())
	if errors.Is(err, errMetricsEmbedNotConfigured) {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	if err != nil {
		// The error names settings only; it never holds the secret value.
		log.WithError(err).Warn("metrics embed: invalid configuration")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "metrics embed configuration is invalid"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"configured": true,
		"url":        embedURL,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

func metricsEmbedFromEnv(getenv func(string) string, now time.Time) (string, time.Time, error) {
	rawURL := strings.TrimSpace(getenv(metricsEmbedURLEnv))
	rawID := strings.TrimSpace(getenv(metricsEmbedDashboardEnv))
	secretFile := strings.TrimSpace(getenv(metricsEmbedSecretFileEnv))
	if rawURL == "" && rawID == "" && secretFile == "" {
		return "", time.Time{}, errMetricsEmbedNotConfigured
	}
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil ||
		base.RawQuery != "" || base.Fragment != "" {
		return "", time.Time{}, fmt.Errorf("%s must be an https base URL", metricsEmbedURLEnv)
	}
	dashboardID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || dashboardID <= 0 {
		return "", time.Time{}, fmt.Errorf("%s must be a positive integer", metricsEmbedDashboardEnv)
	}
	if secretFile == "" {
		return "", time.Time{}, fmt.Errorf("%s is not set", metricsEmbedSecretFileEnv)
	}
	data, err := os.ReadFile(secretFile)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s cannot be read", metricsEmbedSecretFileEnv)
	}
	secret := strings.TrimSpace(string(data))
	if len(secret) < metricsEmbedMinSecretLen {
		return "", time.Time{}, fmt.Errorf("%s holds a too-short secret", metricsEmbedSecretFileEnv)
	}
	expiresAt := now.Add(metricsEmbedLifetime).Truncate(time.Second)
	token, err := signMetabaseEmbedToken([]byte(secret), dashboardID, expiresAt)
	if err != nil {
		return "", time.Time{}, err
	}
	embed := strings.TrimRight(base.String(), "/") + "/embed/dashboard/" + token + "#bordered=true&titled=true"
	return embed, expiresAt, nil
}

// signMetabaseEmbedToken makes the HS256 JWT that Metabase static embedding expects.
func signMetabaseEmbedToken(secret []byte, dashboardID int64, expiresAt time.Time) (string, error) {
	payload, err := json.Marshal(struct {
		Resource map[string]int64 `json:"resource"`
		Params   struct{}         `json:"params"`
		Exp      int64            `json:"exp"`
	}{Resource: map[string]int64{"dashboard": dashboardID}, Exp: expiresAt.Unix()})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}
