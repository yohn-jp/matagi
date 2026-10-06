package desktop

import (
	"crypto/sha256"
	"encoding/hex"
)

const instancePrefix = `Local\Matagi.Desktop.`

// InstanceName returns the per-user kernel mutex name used to ensure one
// Windows-side Matagi runtime/tunnel authority per user.
func InstanceName(userID string) string {
	sum := sha256.Sum256([]byte("matagi-desktop\x00" + userID))
	return instancePrefix + hex.EncodeToString(sum[:12])
}

func ActivateMessageName(userID string) string {
	sum := sha256.Sum256([]byte("matagi-desktop-activate\x00" + userID))
	return "Matagi.Desktop.Activate." + hex.EncodeToString(sum[:12])
}
