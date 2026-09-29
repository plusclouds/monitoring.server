package auth

import (
	"strings"
	"testing"
)

func TestGenerateAndParseKey(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		k, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(k.Full, "mon_"+k.Prefix+"_") || len(k.Full) != 4+8+1+43 {
			t.Fatalf("unexpected key shape %q", k.Full)
		}
		prefix, err := ParseKey(k.Full)
		if err != nil || prefix != k.Prefix {
			t.Fatalf("ParseKey(%q) = %q, %v", k.Full, prefix, err)
		}
		if !MatchKey(k.Full, k.Hash) || MatchKey(k.Full+"x", k.Hash) {
			t.Fatal("MatchKey mismatch")
		}
		if seen[k.Prefix] {
			t.Fatal("duplicate prefix in 100 keys")
		}
		seen[k.Prefix] = true
	}
}

func TestParseKeyRejects(t *testing.T) {
	for _, bad := range []string{
		"", "mon_", "Bearer x", "mon_short_" + strings.Repeat("a", 43),
		"mon_ABCDEFGH_tooShort", "mon_ABCD-FGH_" + strings.Repeat("a", 43),
		"xyz_ABCDEFGH_" + strings.Repeat("a", 43),
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) should fail", bad)
		}
	}
}

func TestGenerateToken(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil || len(tok) != 43 {
		t.Fatalf("GenerateToken() = %q, %v", tok, err)
	}
}
