package agent

import (
	"regexp"
	"testing"
)

func TestNewTokenHasTheIssuedFormatAndItsDigest(t *testing.T) {
	token, digest, err := NewToken("p5")
	if err != nil {
		t.Fatal(err)
	}
	if !tokenPattern("p5").MatchString(token) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) || digest != Digest(token) {
		t.Fatalf("token %q, digest %q", token, digest)
	}
	other, _, err := NewToken("p5")
	if err != nil || other == token {
		t.Fatalf("two tokens are equal: %v", err)
	}
}
