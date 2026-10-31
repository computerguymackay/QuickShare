package main

import (
	"crypto/rand"
	"encoding/base64"
)

// tokenBytes is 144 bits of randomness (24 base64url characters).
const tokenBytes = 18

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// shortToken is the only form of a token that ever reaches the log.
func shortToken(t string) string {
	if len(t) <= 8 {
		return t + "…"
	}
	return t[:8] + "…"
}
