package auth

import (
	"net/http"

	cliproxysession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
)

// ExtractSessionIDs returns the primary and fallback session identities used by
// session affinity. The fallback preserves the first-turn message hash after an
// assistant reply makes the primary hash more specific.
//
// Callers that cache per-conversation state should look up both values and bind
// a fallback hit to the returned primary. This preserves one decision across
// the first-turn/second-turn identity transition.
func ExtractSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (primaryID, fallbackID string) {
	primaryID, fallbackID = extractSessionIDs(headers, payload, metadata)
	if primaryID != "" {
		primaryID = cliproxysession.BoundSessionIdentity(primaryID)
	}
	if fallbackID != "" {
		fallbackID = cliproxysession.BoundSessionIdentity(fallbackID)
	}
	return primaryID, fallbackID
}
