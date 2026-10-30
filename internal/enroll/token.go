package enroll

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"

	"github.com/achmadss/hanse/internal/auth"
)

// JoinTokenPrefix and CredentialPrefix identify the two enrolled values,
// so an operator can tell them apart in output while the value after the
// prefix is random (docs/agent-protocol.md §2.1).
const (
	JoinTokenPrefix  = "hjt_"
	CredentialPrefix = "hnc_"
)

// tokenEntropy is the number of random bytes in one join token or node
// credential: 32 bytes, base64url-encoded without padding, come out as 43
// characters.
const tokenEntropy = 32

// generate returns a fresh value with prefix and the hex SHA-256 hash that
// the store keeps in place of the value.
func generate(prefix string) (plain, hash string, err error) {
	raw := make([]byte, tokenEntropy)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	plain = prefix + base64.RawURLEncoding.EncodeToString(raw)
	return plain, auth.Hash(plain), nil
}

// hashMatches compares a stored hash with the hash of a presented value in
// constant time. A hash that does not decode can only mean a corrupted
// store; it compares as unequal either way.
func hashMatches(stored, presented string) bool {
	storedBytes, err := hex.DecodeString(stored)
	if err != nil {
		storedBytes = nil
	}
	presentedBytes, err := hex.DecodeString(presented)
	if err != nil {
		presentedBytes = []byte{0}
	}
	return subtle.ConstantTimeCompare(storedBytes, presentedBytes) == 1
}
