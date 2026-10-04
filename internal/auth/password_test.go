package auth

import (
	"regexp"
	"testing"
)

// nodeHash was produced by the Nest API's argon2 package for the password "Str0ngPass".
const nodeHash = "$argon2id$v=19$m=19456,p=1,t=2$W2L6nYv9Ejza3S4xrnd4eg$5TKl0n4QcXVu0Nov1Qn+b2oEvY4LtBrxiDJST4fJIUM"

func TestVerifiesAHashMadeByTheNodeServer(t *testing.T) {
	var p Passwords
	if !p.Verify(nodeHash, "Str0ngPass") {
		t.Fatal("a hash from the Nest API must verify here, or users created there could not sign in")
	}
	if p.Verify(nodeHash, "Wr0ngPass") {
		t.Fatal("a wrong password must not verify")
	}
}

func TestHashRoundTripsAndIsStandardPHC(t *testing.T) {
	var p Passwords
	h, err := p.Hash("Str0ngPass")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Verify(h, "Str0ngPass") || p.Verify(h, "nope") {
		t.Fatal("round trip failed")
	}
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`).MatchString(h) {
		t.Fatalf("unexpected hash format: %s", h)
	}
}

func TestUnknownAccountsStillCostAHashCheck(t *testing.T) {
	var p Passwords
	if p.Verify("", "anything") {
		t.Fatal("an empty hash must never verify")
	}
}
