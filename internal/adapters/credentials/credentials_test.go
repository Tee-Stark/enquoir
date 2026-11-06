package credentials

import (
	"strings"
	"testing"

	"enquoir/internal/app"
)

func TestPBKDF2_HashThenVerify(t *testing.T) {
	h := &PBKDF2Hasher{Iterations: 1000}
	encoded, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "correct horse") {
		t.Fatal("encoded hash contains the password")
	}
	if ok, err := h.Verify("correct horse battery", encoded); !ok || err != nil {
		t.Errorf("Verify(right) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := h.Verify("correct horse batterz", encoded); ok || err != nil {
		t.Errorf("Verify(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestPBKDF2_SaltsEveryHash(t *testing.T) {
	h := &PBKDF2Hasher{Iterations: 1000}
	a, _ := h.Hash("same password")
	b, _ := h.Hash("same password")
	if a == b {
		t.Error("two hashes of the same password are identical: no salt")
	}
}

// The iteration count travels with the hash, so raising it later
// still verifies passwords hashed before.
func TestPBKDF2_VerifiesWithTheHashsOwnIterations(t *testing.T) {
	old, _ := (&PBKDF2Hasher{Iterations: 1000}).Hash("correct horse battery")
	if ok, err := (&PBKDF2Hasher{Iterations: 5000}).Verify("correct horse battery", old); !ok || err != nil {
		t.Errorf("Verify with a newer iteration count = %v, %v; want true, nil", ok, err)
	}
}

func TestPBKDF2_DefaultsToOWASPIterations(t *testing.T) {
	encoded, _ := (&PBKDF2Hasher{}).Hash("correct horse battery")
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$600000$") {
		t.Errorf("encoded = %q, want pbkdf2-sha256 at 600000 iterations", encoded)
	}
}

// app.DummyPasswordHash must take the full verification path, or a
// login for an unknown email would answer faster than a wrong password.
func TestPBKDF2_DummyHashIsWellFormed(t *testing.T) {
	ok, err := (&PBKDF2Hasher{}).Verify("anything", app.DummyPasswordHash)
	if ok || err != nil {
		t.Errorf("Verify(dummy) = %v, %v; want false, nil (parsed and computed)", ok, err)
	}
}

func TestPBKDF2_RejectsMalformedHashes(t *testing.T) {
	h := &PBKDF2Hasher{Iterations: 1000}
	for _, bad := range []string{"", "plain", "bcrypt$10$x$y", "pbkdf2-sha256$abc$AAAA$AAAA", "pbkdf2-sha256$1000$!!$AAAA"} {
		if _, err := h.Verify("pw", bad); err == nil {
			t.Errorf("Verify(%q): want an error", bad)
		}
	}
}

func TestRandomIDsAndSecrets(t *testing.T) {
	ids, secrets := RandomIDs{}, RandomSecrets{}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, s := ids.NewID("org"), secrets.NewSecret("ses")
		if !strings.HasPrefix(id, "org_") || len(id) != len("org_")+24 {
			t.Fatalf("id %q: want org_ followed by 24 hex characters", id)
		}
		if !strings.HasPrefix(s, "ses_") || len(s) < len("ses_")+43 {
			t.Fatalf("secret %q: want ses_ followed by at least 256 bits", s)
		}
		if seen[id] || seen[s] {
			t.Fatal("generated a duplicate")
		}
		seen[id], seen[s] = true, true
	}
}
