package openai

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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// codexUsageLimitMinResetHorizon backstops the rewrite: even when every
// candidate's cooldown qualifies as genuine usage exhaustion (see
// AllUsageExhausted), a reset less than this far away is presented
// unchanged. A real Codex usage limit resets hours to days out; a reset
// this close is a signal the classification above still let a short-lived
// condition through, and a sub-5-minute "usage_limit_reached" would mislead
// the Codex CLI's retry/backoff handling more than the generic error would.
const codexUsageLimitMinResetHorizon = 5 * time.Minute

// rewriteQuotaExhaustedError inspects an auth-selection failure for the
// Responses route and, only when modelName resolves to the Codex provider
// alone and every candidate Codex auth for it is currently blocked
// exclusively by genuine, credential-scoped usage exhaustion (not merely a
// quota/rate-limit cooldown; see QuotaUnavailabilitySummary.AllUsageExhausted)
// with an earliest reset at least codexUsageLimitMinResetHorizon away,
// replaces it with an OpenAI/Codex-shaped 429 "usage_limit_reached" response,
// the same shape the official Codex CLI gets from OpenAI itself when a
// single account's usage limit is hit. This lets the Codex CLI show its
// native "you've hit your usage limit, try again at <time>" message and
// backoff instead of a generic retry-then-give-up error. Any other "no auth
// available" shape (a disabled auth, a missing auth, a mix of quota and
// non-quota cooldowns, a transient quota/rate-limit backoff that is not
// actually a usage limit, a reset too soon to trust, a model that can also
// route to a non-Codex provider, or any other error) is returned unchanged.
// This never changes cooldown or selection behavior; it only changes how an
// already-decided failure is presented.
func (h *OpenAIResponsesAPIHandler) rewriteQuotaExhaustedError(modelName string, errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg == nil || h == nil || h.AuthManager == nil || !isNoCodexAuthAvailableError(errMsg) || !codexIsSoleProviderForModel(modelName) {
		return errMsg
	}
	now := time.Now()
	summary := h.AuthManager.SummarizeModelQuotaUnavailability("codex", modelName, now)
	if !summary.Applicable || !summary.AllUsageExhausted || summary.EarliestReset.IsZero() {
		return errMsg
	}
	if summary.EarliestReset.Sub(now) < codexUsageLimitMinResetHorizon {
		return errMsg
	}
	return codexUsageLimitErrorMessage(modelName, summary, now, errMsg.Error)
}

// isNoCodexAuthAvailableError reports whether errMsg represents an
// auth-selection failure (no candidate auth was usable for the request),
// rather than an upstream or request error. These are the only failures this
// rewrite may reinterpret. It mirrors the Claude Messages route's equivalent
// check; the two live in separate packages so each provider's route can be
// read and reviewed on its own.
func isNoCodexAuthAvailableError(errMsg *interfaces.ErrorMessage) bool {
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

// codexIsSoleProviderForModel reports whether modelName resolves to exactly
// one provider and that provider is Codex. A model that can also be served by
// another provider is left out of this rewrite: "every Codex account is
// quota-blocked" is not the same claim as "no auth was available", and
// presenting a Codex-specific usage_limit_reached body would misrepresent a
// failure that may be unrelated to Codex quota. This mirrors the provider
// resolution ExecuteWithAuthManager itself uses for the common case, without
// the plugin-router/model-router/Home-routing special cases: those routes
// have their own failure presentation and simply fall through to the
// original, unrewritten error here.
func codexIsSoleProviderForModel(modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return false
	}
	baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
	providers := util.GetProviderName(baseModel)
	if len(providers) == 0 && baseModel != modelName {
		providers = util.GetProviderName(modelName)
	}
	return len(providers) == 1 && strings.EqualFold(strings.TrimSpace(providers[0]), "codex")
}

// codexUsageLimitErrorBody is the shape OpenAI itself sends for a Codex
// usage-limit 429, e.g.:
//
//	{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached",
//	"plan_type":"team","resets_at":1791412966,"eligible_promo":null,
//	"limit_window_minutes":300,"resets_in_seconds":2837}}
//
// plan_type is intentionally omitted: a pooled proxy has no single plan, and
// the Codex CLI's Display for UsageLimitReachedError (codex-rs
// rust-v0.160.1) turns a known plan_type into plan-specific recovery copy
// ("send a request to your admin" for a team plan, "upgrade to Pro" for
// Plus, etc.) that does not apply to a shared pool. With no plan_type (and no
// promo/rate-limit-reached-type/limit_name, none of which this proxy can
// speak for across pooled accounts either), the CLI falls back to its
// generic "You've hit your usage limit. Try again at <local time>." message,
// which is accurate here.
type codexUsageLimitErrorBody struct {
	Type               string `json:"type"`
	Message            string `json:"message"`
	ResetsAt           int64  `json:"resets_at,omitempty"`
	ResetsInSeconds    int64  `json:"resets_in_seconds,omitempty"`
	LimitWindowMinutes int    `json:"limit_window_minutes,omitempty"`
}

type codexUsageLimitErrorResponse struct {
	Error codexUsageLimitErrorBody `json:"error"`
}

// codexUsageLimitErrorMessage builds the OpenAI-shaped 429 response for a
// confirmed all-Codex-accounts-quota-exhausted condition.
func codexUsageLimitErrorMessage(modelName string, summary coreauth.QuotaUnavailabilitySummary, now time.Time, cause error) *interfaces.ErrorMessage {
	resetAt := summary.EarliestReset.UTC()
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "the requested model"
	}
	message := fmt.Sprintf("All accounts on this proxy have reached their usage limit for %s. The earliest resets at %s.", modelName, resetAt.Format(time.RFC3339))

	resetsInSeconds := int64(math.Ceil(summary.EarliestReset.Sub(now).Seconds()))
	if resetsInSeconds < 0 {
		resetsInSeconds = 0
	}

	body, errMarshal := json.Marshal(codexUsageLimitErrorResponse{
		Error: codexUsageLimitErrorBody{
			Type:               "usage_limit_reached",
			Message:            message,
			ResetsAt:           resetAt.Unix(),
			ResetsInSeconds:    resetsInSeconds,
			LimitWindowMinutes: summary.WindowMinutes,
		},
	})
	if errMarshal != nil {
		body = []byte(`{"error":{"type":"usage_limit_reached","message":"All accounts on this proxy have reached their usage limit."}}`)
	}

	retrySeconds := resetsInSeconds
	if retrySeconds < 1 {
		retrySeconds = 1
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", strconv.FormatInt(retrySeconds, 10))

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
