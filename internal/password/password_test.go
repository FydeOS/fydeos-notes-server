// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package password

import "testing"

const legacyPasswordFixture = "$argon2id$v=19$m=19456,t=2,p=1$/JC3Ue87NEBXtjra7TY9TQ$oysAbNozbP/Z6kdbyPXYDRcZFr4WJlFEHhx+88QRjoc"

func TestHashUsesLegacyCompatibleArgon2idFormat(t *testing.T) {
	encoded, err := Hash("p4-password")
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(encoded, "p4-password") {
		t.Fatal("generated hash did not verify")
	}
	if Verify(encoded, "wrong-password") {
		t.Fatal("generated hash accepted an incorrect password")
	}
	if len(encoded) == 0 || encoded[:len("$argon2id$v=19$m=19456,t=2,p=1$")] != "$argon2id$v=19$m=19456,t=2,p=1$" {
		t.Fatalf("generated password hash has incompatible parameters: %q", encoded)
	}
}

func TestVerifyMatchesLegacyRustArgon2Fixture(t *testing.T) {
	if !Verify(legacyPasswordFixture, "p4-password") {
		t.Fatal("Go verifier rejected the legacy Rust Argon2 fixture")
	}
	if Verify(legacyPasswordFixture, "wrong-password") {
		t.Fatal("Go verifier accepted an incorrect password")
	}
}

func TestVerifyRejectsMalformedOrExpensiveHashes(t *testing.T) {
	for _, encoded := range []string{
		"not-a-password-hash",
		"$argon2d$v=19$m=19456,t=2,p=1$/JC3Ue87NEBXtjra7TY9TQ$oysAbNozbP/Z6kdbyPXYDRcZFr4WJlFEHhx+88QRjoc",
		"$argon2id$v=19$m=999999999,t=2,p=1$/JC3Ue87NEBXtjra7TY9TQ$oysAbNozbP/Z6kdbyPXYDRcZFr4WJlFEHhx+88QRjoc",
	} {
		if Verify(encoded, "p4-password") {
			t.Errorf("Verify accepted malformed or unsupported hash %q", encoded)
		}
	}
}
