package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func key(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpen(t *testing.T) {
	k, err := NewKeyring("a", map[string][]byte{"a": key(t)})
	if err != nil {
		t.Fatal(err)
	}
	aad := AAD("tenant", "cred", "snmp_v3")
	s, err := k.Seal([]byte("s3cret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.Ciphertext, []byte("s3cret")) || s.KEKID != "a" {
		t.Fatalf("unexpected sealed value: %+v", s)
	}
	got, err := k.Open(s, aad)
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

// ADR-0006: a ciphertext moved to another tenant or credential does not decrypt.
func TestOpenRejectsOtherOwner(t *testing.T) {
	k, _ := NewKeyring("a", map[string][]byte{"a": key(t)})
	s, _ := k.Seal([]byte("x"), AAD("tenant-1", "cred", "redfish"))
	if _, err := k.Open(s, AAD("tenant-2", "cred", "redfish")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
	s.Ciphertext[0] ^= 1
	if _, err := k.Open(s, AAD("tenant-1", "cred", "redfish")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: err = %v, want ErrDecrypt", err)
	}
}

func TestRotation(t *testing.T) {
	oldKey, newKey := key(t), key(t)
	before, _ := NewKeyring("old", map[string][]byte{"old": oldKey})
	aad := AAD("t", "c", "ipmi")
	s, _ := before.Seal([]byte("pw"), aad)

	after, _ := NewKeyring("new", map[string][]byte{"old": oldKey, "new": newKey})
	if got, err := after.Open(s, aad); err != nil || string(got) != "pw" {
		t.Fatalf("old value under new keyring: %q, %v", got, err)
	}
	re, err := after.Rewrap(s)
	if err != nil || re.KEKID != "new" || !bytes.Equal(re.Ciphertext, s.Ciphertext) {
		t.Fatalf("Rewrap = %+v, %v", re, err)
	}
	onlyNew, _ := NewKeyring("new", map[string][]byte{"new": newKey})
	if got, err := onlyNew.Open(re, aad); err != nil || string(got) != "pw" {
		t.Fatalf("rewrapped value: %q, %v", got, err)
	}
	if _, err := onlyNew.Open(s, aad); !errors.Is(err, ErrUnknownKEK) {
		t.Fatalf("err = %v, want ErrUnknownKEK", err)
	}
}

func TestNewKeyringRequiresActive(t *testing.T) {
	if _, err := NewKeyring("x", map[string][]byte{"a": key(t)}); err == nil {
		t.Fatal("want error for missing active key")
	}
	if _, err := NewKeyring("a", map[string][]byte{"a": []byte("short")}); err == nil {
		t.Fatal("want error for a short key")
	}
}
