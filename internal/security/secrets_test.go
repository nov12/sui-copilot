package security

import "testing"

func TestSealOpen(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	ciphertext, err := Seal(key, []byte("cookie"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := Open(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "cookie" {
		t.Fatalf("unexpected plaintext: %q", plaintext)
	}
}
