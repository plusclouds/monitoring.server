// Package crypto implements envelope encryption for secrets at rest
// (ADR-0006): each secret is encrypted with its own random data key, and the
// data key is wrapped by a key-encryption key (KEK) held outside the
// database. Both layers use AES-256-GCM.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/plusclouds/monitoring.server/internal/config"
)

// KeySize is the size of every key: KEKs and data keys.
const KeySize = 32

// ErrUnknownKEK means a sealed value names a KEK this process does not have.
var ErrUnknownKEK = errors.New("key-encryption key not configured")

// ErrDecrypt means a value could not be opened: wrong key, tampered
// ciphertext, or associated data that does not match (a ciphertext moved to
// another tenant or credential).
var ErrDecrypt = errors.New("cannot decrypt secret")

// Sealed is an encrypted value as stored in the database.
type Sealed struct {
	Ciphertext []byte // AES-GCM output, tag included
	Nonce      []byte
	WrappedDEK []byte // nonce || AES-GCM(KEK, data key)
	KEKID      string
}

// Keyring holds the configured KEKs. The active one wraps new data keys;
// every one unwraps.
type Keyring struct {
	active string
	keys   map[string]cipher.AEAD
}

// NewKeyring builds a keyring from raw 32-byte keys.
func NewKeyring(active string, keys map[string][]byte) (*Keyring, error) {
	if _, ok := keys[active]; !ok {
		return nil, fmt.Errorf("active key %q is not in the keyring", active)
	}
	k := &Keyring{active: active, keys: make(map[string]cipher.AEAD, len(keys))}
	for id, raw := range keys {
		a, err := newAEAD(raw)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", id, err)
		}
		k.keys[id] = a
	}
	return k, nil
}

// Load reads the KEKs named by the process config. Each is 32 random bytes,
// base64-encoded, in a file or an environment variable.
func Load(c config.Crypto) (*Keyring, error) {
	if c.KeyProvider != "file" {
		return nil, fmt.Errorf("crypto.key_provider %q is not supported", c.KeyProvider)
	}
	keys := make(map[string][]byte, len(c.File.Keys))
	for _, ref := range c.File.Keys {
		var encoded string
		switch {
		case ref.Path != "":
			b, err := os.ReadFile(ref.Path) //nolint:gosec // key files are named in the operator's config
			if err != nil {
				return nil, fmt.Errorf("crypto key %q: %w", ref.ID, err)
			}
			encoded = string(b)
		default:
			encoded = os.Getenv(ref.Env)
			if encoded == "" {
				return nil, fmt.Errorf("crypto key %q: environment variable %s is empty", ref.ID, ref.Env)
			}
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil || len(raw) != KeySize {
			return nil, fmt.Errorf("crypto key %q: want %d bytes, base64-encoded", ref.ID, KeySize)
		}
		keys[ref.ID] = raw
	}
	return NewKeyring(c.File.Active, keys)
}

// Active is the ID of the KEK that wraps new data keys.
func (k *Keyring) Active() string { return k.active }

// Seal encrypts plaintext. aad binds the ciphertext to its owner (tenant,
// object ID, type); Open must be given the same aad.
func (k *Keyring) Seal(plaintext, aad []byte) (Sealed, error) {
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, err
	}
	data, err := newAEAD(dek)
	if err != nil {
		return Sealed{}, err
	}
	nonce, err := randomNonce(data)
	if err != nil {
		return Sealed{}, err
	}
	kek := k.keys[k.active]
	wrapNonce, err := randomNonce(kek)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{
		Ciphertext: data.Seal(nil, nonce, plaintext, aad),
		Nonce:      nonce,
		WrappedDEK: kek.Seal(wrapNonce, wrapNonce, dek, []byte(k.active)),
		KEKID:      k.active,
	}, nil
}

// Open decrypts a sealed value.
func (k *Keyring) Open(s Sealed, aad []byte) ([]byte, error) {
	dek, err := k.unwrap(s)
	if err != nil {
		return nil, err
	}
	data, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	if len(s.Nonce) != data.NonceSize() {
		return nil, ErrDecrypt
	}
	out, err := data.Open(nil, s.Nonce, s.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

// Rewrap wraps the data key of s with the active KEK, leaving the ciphertext
// as it is. It is a no-op for values already wrapped by the active KEK.
func (k *Keyring) Rewrap(s Sealed) (Sealed, error) {
	if s.KEKID == k.active {
		return s, nil
	}
	dek, err := k.unwrap(s)
	if err != nil {
		return Sealed{}, err
	}
	kek := k.keys[k.active]
	nonce, err := randomNonce(kek)
	if err != nil {
		return Sealed{}, err
	}
	s.WrappedDEK, s.KEKID = kek.Seal(nonce, nonce, dek, []byte(k.active)), k.active
	return s, nil
}

func (k *Keyring) unwrap(s Sealed) ([]byte, error) {
	kek, ok := k.keys[s.KEKID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKEK, s.KEKID)
	}
	n := kek.NonceSize()
	if len(s.WrappedDEK) < n {
		return nil, ErrDecrypt
	}
	dek, err := kek.Open(nil, s.WrappedDEK[:n], s.WrappedDEK[n:], []byte(s.KEKID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return dek, nil
}

// AAD joins the fields that bind a ciphertext to its owner, separated by a
// byte that cannot appear in them.
func AAD(parts ...string) []byte { return []byte(strings.Join(parts, "\x1f")) }

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("key must be %d bytes", KeySize)
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func randomNonce(a cipher.AEAD) ([]byte, error) {
	n := make([]byte, a.NonceSize())
	_, err := rand.Read(n)
	return n, err
}

// GenerateKey returns a new random KEK, base64-encoded, for
// `monitor admin generate-kek`.
func GenerateKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
