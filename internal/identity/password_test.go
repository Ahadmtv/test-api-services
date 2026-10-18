package identity

import (
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	password := "secret1234"
	encoded, err := Hash(password, DefaultParams)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if encoded == "" {
		t.Fatal("encoded hash is empty")
	}
	if encoded == password {
		t.Fatal("encoded hash equals plaintext password")
	}

	ok, err := Verify(password, encoded)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("valid password should verify")
	}

	ok, err = Verify("wrongpassword", encoded)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password should not verify")
	}

	// Malformed hash should error, not panic.
	_, err = Verify("x", "not-a-valid-hash")
	if err == nil {
		t.Error("expected error for malformed hash")
	}
	_, err = Verify("x", "$argon2id$v=99$m=1,t=1,p=1$abc$def")
	if err == nil {
		t.Error("expected error for unsupported argon2 version")
	}
}

func TestHashDeterministicParams(t *testing.T) {
	params := Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}
	h1, err := Hash("pw", params)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Hash("pw", params)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("two hashes of the same password should differ due to random salt")
	}
	ok, _ := Verify("pw", h1)
	if !ok {
		t.Error("h1 should verify")
	}
	ok, _ = Verify("pw", h2)
	if !ok {
		t.Error("h2 should verify")
	}
}
