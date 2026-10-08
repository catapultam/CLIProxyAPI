package auth

import (
	"strconv"
	"strings"
)

// AttributeSize is the routing attribute key for a credential's hand-set size.
const AttributeSize = "size"

// AttributeFileSize marks size values inherited from a physical auth file.
const AttributeFileSize = "file_size"

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
	var size float64
	switch value := rawSize.(type) {
	case float64:
		size = value
	case string:
		trimmed := strings.TrimSpace(value)
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		if errParse != nil {
			return
		}
		size = parsed
	default:
		return
	}
	if size <= 0 {
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
