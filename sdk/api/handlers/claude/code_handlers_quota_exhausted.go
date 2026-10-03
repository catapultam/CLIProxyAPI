package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// rewriteQuotaExhaustedError inspects an auth-selection failure for the Claude
// Messages route and, only when every candidate claude auth for modelName is
// currently blocked exclusively by quota/rate-limit cooldown, replaces it with
// an Anthropic-shaped 429 response carrying the unified rate-limit headers
// Claude Code understands natively. Any other "no auth available" shape (a
// disabled auth, a missing auth, a mix of quota and non-quota cooldowns, or
// any other error) is returned unchanged. This never changes cooldown or
// selection behavior; it only changes how an already-decided failure is
// presented.
func (h *ClaudeCodeAPIHandler) rewriteQuotaExhaustedError(modelName string, errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg == nil || h.AuthManager == nil || !isNoClaudeAuthAvailableError(errMsg) {
		return errMsg
	}
	// Execution ran on the rewritten model, so its quota state is the one to report.
	modelName = h.RewriteModelName(modelName)
	now := time.Now()
	summary := h.AuthManager.SummarizeModelQuotaUnavailability(h.HandlerType(), modelName, now)
	if !summary.Applicable || !summary.AllQuotaCooldown || summary.EarliestReset.IsZero() {
		return errMsg
	}
	return quotaExhaustedErrorMessage(modelName, summary, now, errMsg.Error)
}

// isNoClaudeAuthAvailableError reports whether errMsg represents an
// auth-selection failure (no candidate auth was usable for the request),
// rather than an upstream or request error. These are the only failures this
// rewrite may reinterpret.
func isNoClaudeAuthAvailableError(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	if errMsg.StatusCode != http.StatusServiceUnavailable && errMsg.StatusCode != http.StatusTooManyRequests {
		return false
	}
	type modelCooldownMarker interface {
		IsModelCooldown() bool
	}
	var cooldownMarker modelCooldownMarker
	if errors.As(errMsg.Error, &cooldownMarker) && cooldownMarker != nil && cooldownMarker.IsModelCooldown() {
		return true
	}
	var authErr *coreauth.Error
	if errors.As(errMsg.Error, &authErr) && authErr != nil {
		code := strings.TrimSpace(authErr.Code)
		return code == "auth_not_found" || code == "auth_unavailable"
	}
	return false
}

// quotaExhaustedErrorMessage builds the Anthropic-shaped 429 response for a
// confirmed all-accounts-quota-exhausted condition.
func quotaExhaustedErrorMessage(modelName string, summary coreauth.QuotaUnavailabilitySummary, now time.Time, cause error) *interfaces.ErrorMessage {
	resetAt := summary.EarliestReset.UTC()
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "the requested model"
	}
	message := fmt.Sprintf("All accounts on this proxy have reached their usage limit for %s. The earliest resets at %s.", modelName, resetAt.Format(time.RFC3339))

	body, errMarshal := json.Marshal(claudeErrorResponse{
		Type: "error",
		Error: claudeErrorDetail{
			Type:    "rate_limit_error",
			Message: message,
		},
	})
	if errMarshal != nil {
		body = []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"All accounts on this proxy have reached their usage limit."}}`)
	}

	retrySeconds := int64(math.Ceil(summary.EarliestReset.Sub(now).Seconds()))
	if retrySeconds < 1 {
		retrySeconds = 1
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(resetAt.Unix(), 10))
	if claim := strings.TrimSpace(summary.RepresentativeClaim); claim != "" {
		headers.Set("Anthropic-Ratelimit-Unified-Representative-Claim", claim)
	}
	headers.Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
	headers.Set("X-Should-Retry", "false")

	if cause == nil {
		cause = errors.New(message)
	}
	return &interfaces.ErrorMessage{
		StatusCode:     http.StatusTooManyRequests,
		Error:          cause,
		DirectResponse: true,
		Body:           body,
		Headers:        headers,
	}
}
