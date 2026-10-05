package out

import (
	"encoding/json"
	"io"
)

// WriteDraftCleanupError retains filesystem effect knowledge in the new local
// cleanup command's legacy JSON failure, without changing other legacy errors.
func WriteDraftCleanupError(w io.Writer, failure *AgentError) error {
	return json.NewEncoder(w).Encode(struct {
		Success bool        `json:"success"`
		Error   *AgentError `json:"error"`
	}{false, failure})
}
