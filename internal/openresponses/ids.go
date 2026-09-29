package openresponses

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns prefix + "_" + 24 random hex characters, e.g. "resp_…".
func NewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
