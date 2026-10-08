package auth

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// AttributeSize is the routing attribute key for a credential's hand-set size.
const AttributeSize = "size"

// AttributeFileSize marks size values inherited from a physical auth file.
const AttributeFileSize = "file_size"

// parseSizeValue is the single validator ApplyAuthSizeMetadata and authSize
// both use to turn a JSON-compatible metadata value (or a routing attribute
// string) into a size. A value is valid only when it is a finite number
// strictly greater than zero: strconv.ParseFloat happily parses "NaN",
// "inf", "-inf", and "Infinity" without error, so those are rejected
// explicitly (via math.IsNaN/math.IsInf) rather than silently treated as a
// huge or undefined "known" size. Anything else invalid, non-numeric, or
// non-positive reports false, the same "unknown" outcome authSize already
// used for a missing or garbage attribute.
func parseSizeValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return finitePositiveSize(typed)
	case float32:
		return finitePositiveSize(float64(typed))
	case int:
		return finitePositiveSize(float64(typed))
	case int64:
		return finitePositiveSize(float64(typed))
	case json.Number:
		parsed, errParse := typed.Float64()
		if errParse != nil {
			return 0, false
		}
		return finitePositiveSize(parsed)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		if errParse != nil {
			return 0, false
		}
		return finitePositiveSize(parsed)
	default:
		return 0, false
	}
}

// finitePositiveSize rejects NaN, +/-Inf, and non-positive sizes.
func finitePositiveSize(size float64) (float64, bool) {
	if math.IsNaN(size) || math.IsInf(size, 0) || size <= 0 {
		return 0, false
	}
	return size, true
}

// ApplyAuthSizeMetadata copies a valid file size into an auth's metadata and
// routing attributes. size is an optional, hand-set number (plan seat count,
// relative quota share, or any other unit the owner picks consistently
// across their credentials) that biases next-reset toward spending smaller
// accounts first; see authSize and sortNextReset. A missing, non-numeric, or
// non-positive value is ignored and leaves any existing attribute in place,
// mirroring ApplyAuthPriorityMetadata.
func ApplyAuthSizeMetadata(auth *Auth, metadata map[string]any) {
	if auth == nil {
		return
	}
	delete(auth.Attributes, AttributeFileSize)
	rawSize, ok := metadata["size"]
	if !ok {
		return
	}
	size, ok := parseSizeValue(rawSize)
	if !ok {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["size"] = rawSize
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[AttributeSize] = strconv.FormatFloat(size, 'g', -1, 64)
	auth.Attributes[AttributeFileSize] = "true"
}
