package auth

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// isUsageRestrictionError identifies account restrictions independently of explicit
// request-scoped policy: that policy may skip cooling, but must not retain affinity.
func isUsageRestrictionError(err error) bool {
	if usageRestrictionStatusCode(err) != 0 {
		return true
	}
	// Fast-mode credit eligibility remains request-scoped for retry/cooling, but
	// it still proves the selected account cannot serve this session's request.
	body := billingErrorBody(err)
	return strings.EqualFold(strings.TrimSpace(gjson.Get(body, "error.type").String()), "invalid_request_error") &&
		strings.Contains(strings.ToLower(gjson.Get(body, "error.message").String()), "usage credits are required for fast mode")
}

// usageRestrictionStatusCode returns the internal payment/quota classification.
// Only exact structured identifiers and known billing messages are recognized;
// request limits such as context length and max_tokens are not account restrictions.
func usageRestrictionStatusCode(err error) int {
	if err == nil {
		return 0
	}
	status := statusCodeFromError(err)
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return status
	}
	body := billingErrorBody(err)
	if body == "" {
		return 0
	}
	if isBillingErrorBody(body) {
		return http.StatusPaymentRequired
	}
	for _, path := range []string{"error.type", "error.code", "error.status"} {
		switch strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String())) {
		case "billing_error":
			return http.StatusPaymentRequired
		case "usage_limit_reached", "insufficient_quota", "quota_exceeded",
			"rate_limit_exceeded", "rate_limit_error", "resource_exhausted":
			return http.StatusTooManyRequests
		}
	}
	return 0
}
