// Package auth creates and checks API keys (ADR-0007).
//
// A key looks like mon_<prefix>_<secret>: an 8-character base62 prefix that
// finds the row, and 32 random bytes in base62. Only SHA-256 of the full key
// is stored; keys are high-entropy, so a slow hash is not needed.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"math/big"
	"strings"
)

const (
	keyScheme    = "mon_"
	prefixLength = 8
	secretBytes  = 32
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrMalformedKey means the presented value is not shaped like an API key.
var ErrMalformedKey = errors.New("malformed API key")

// NewKey is a freshly generated key. Full is shown once and never stored.
type NewKey struct {
	Full   string
	Prefix string
	Hash   []byte
}

// GenerateKey returns a new random API key.
func GenerateKey() (NewKey, error) {
	prefix, err := randomBase62(prefixLength)
	if err != nil {
		return NewKey{}, err
	}
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return NewKey{}, err
	}
	full := keyScheme + prefix + "_" + encodeBase62(b)
	return NewKey{Full: full, Prefix: prefix, Hash: HashKey(full)}, nil
}

// HashKey is the stored form of a key.
func HashKey(full string) []byte {
	h := sha256.Sum256([]byte(full))
	return h[:]
}

// ParseKey returns the lookup prefix of a presented key.
func ParseKey(full string) (prefix string, err error) {
	rest, ok := strings.CutPrefix(full, keyScheme)
	if !ok {
		return "", ErrMalformedKey
	}
	prefix, secret, ok := strings.Cut(rest, "_")
	if !ok || len(prefix) != prefixLength || len(secret) < 40 || !isBase62(prefix) || !isBase62(secret) {
		return "", ErrMalformedKey
	}
	return prefix, nil
}

// MatchKey compares a presented key with a stored hash in constant time.
func MatchKey(full string, stored []byte) bool {
	return subtle.ConstantTimeCompare(HashKey(full), stored) == 1
}

// GenerateToken returns a random base62 token of 43 characters (256 bits),
// for status and preshared enrollment tokens (`monitor admin gen-token`).
func GenerateToken() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return encodeBase62(b), nil
}

func randomBase62(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(base62)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = base62[v.Int64()]
	}
	return string(out), nil
}

// encodeBase62 encodes b as a fixed-width base62 string (43 characters for 32 bytes).
func encodeBase62(b []byte) string {
	width := (len(b)*8*1000 + 5953) / 5954 // ceil(bits / log2(62))
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	out := make([]byte, width)
	mod := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = base62[mod.Int64()]
	}
	return string(out)
}

func isBase62(s string) bool {
	for i := range len(s) {
		if !strings.ContainsRune(base62, rune(s[i])) {
			return false
		}
	}
	return true
}
