package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// isBillingError recognizes account balance refusals sent as HTTP 400 by
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
	typeName := strings.ToLower(strings.TrimSpace(gjson.Get(body, "error.type").String()))
	if typeName == "billing_error" {
		return true
	}
	if typeName != "invalid_request_error" {
		return false
	}
	message := strings.ToLower(gjson.Get(body, "error.message").String())
	return strings.Contains(message, "credit balance is too low")
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

// cooldownStatusCodeFromResult applies the existing payment cooldown policy
// without rewriting the upstream HTTP status or error body.
func cooldownStatusCodeFromResult(err *Error) int {
	if isBillingError(err) {
		return http.StatusPaymentRequired
	}
	return statusCodeFromResult(err)
}
