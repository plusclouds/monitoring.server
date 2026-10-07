package auth

import (
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	p := PasswordParams{MemoryKiB: 1024, Iterations: 1, Parallelism: 1}
	h, err := HashPassword("sensor-secret", p)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("hash %q %v", h, err)
	}
	if ok, err := VerifyPassword("sensor-secret", h); !ok || err != nil {
		t.Errorf("right password: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("sensor-secret", p)
	if h2 == h {
		t.Error("no salt")
	}
	for _, bad := range []string{"", "$argon2i$v=19$m=1,t=1,p=1$a$b", "$argon2id$v=19$m=99999999,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=1024,t=1,p=1$!!$AAAA"} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
