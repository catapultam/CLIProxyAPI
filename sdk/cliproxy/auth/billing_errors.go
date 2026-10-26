package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// isBillingError recognizes account balance and usage limit refusals sent as HTTP 400 by
// Claude and compatible gateways. Ordinary malformed requests must still stop
// credential rotation, and fast-mode entitlement errors remain request-scoped.
func isBillingError(err error) bool {
	if err == nil || statusCodeFromError(err) != http.StatusBadRequest || isRequestScopedError(err) {
		return false
	}
	body := billingErrorBody(err)
	if !gjson.Valid(body) {
		return false
	}
	return isBillingErrorBody(body)
}

func isBillingErrorBody(body string) bool {
	typeName := strings.ToLower(strings.TrimSpace(gjson.Get(body, "error.type").String()))
	if typeName == "billing_error" {
		return true
	}
	if typeName != "invalid_request_error" {
		return false
	}
	message := strings.ToLower(gjson.Get(body, "error.message").String())
	return strings.Contains(message, "credit balance is too low") ||
		strings.Contains(message, "reached your specified api usage limits")
}

func billingErrorBody(err error) string {
	for err != nil {
		body := extractErrorBody(err)
		if gjson.Valid(body) {
			return body
		}
		err = errors.Unwrap(err)
	}
	return ""
}

// cooldownStatusCodeFromResult applies payment or quota cooldowns to recognized
// usage restrictions without rewriting the upstream HTTP status or error body.
func cooldownStatusCodeFromResult(err *Error) int {
	if usageStatus := usageRestrictionStatusCode(err); usageStatus != 0 {
		return usageStatus
	}
	return statusCodeFromResult(err)
}
