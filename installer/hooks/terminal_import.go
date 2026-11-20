package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// TerminalImportFingerprint returns the fingerprint that release v2 recorded
// for the reviewed values in the Windows Terminal settings data. The import
// from release v2 compares it with the receipt of release v2. It is empty
// for settings that hold no reviewed value.
func TerminalImportFingerprint(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	raw, err := terminalExtract(data)
	if err != nil || len(raw) == 0 {
		return "", err
	}
	owned, err := terminalOwned(raw)
	if err != nil || len(owned) == 0 {
		return "", err
	}
	encoded, err := json.Marshal([]map[string]terminalCanonicalValue{owned})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
