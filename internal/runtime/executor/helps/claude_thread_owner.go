package helps

import (
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// RecordClaudeThreadOwner remembers that auth produced the Claude message messageID
// when the upstream request used a thread, so a later thread continuation from that
// message can be routed back to the credential that holds the thread state.
func RecordClaudeThreadOwner(upstreamBody []byte, messageID string, auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(messageID) == "" {
		return
	}
	if !gjson.GetBytes(upstreamBody, "thread").IsObject() {
		return
	}
	cliproxyauth.RecordClaudeThreadOwner(messageID, auth.ID)
}
