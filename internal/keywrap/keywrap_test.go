package keywrap

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
)

func TestWrapAndUnwrap(t *testing.T) {
	p, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	answer, err := Wrap(p.PublicLine(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Unwrap(answer)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("round trip failed", err)
	}
	forms := [][]byte{key, []byte(hex.EncodeToString(key)), []byte(base64.StdEncoding.EncodeToString(key)), []byte(base64.RawStdEncoding.EncodeToString(key)), []byte(base64.URLEncoding.EncodeToString(key)), []byte(base64.RawURLEncoding.EncodeToString(key))}
	for _, form := range forms {
		if bytes.Contains(answer, form) {
			t.Fatal("answer disclosed a key form")
		}
	}
	if !bytes.HasPrefix(answer, []byte("berthy-key-wrap/1 ok ")) {
		t.Fatal("invalid wrapped answer")
	}
}
func TestMalformedWire(t *testing.T) {
	p, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range [][]byte{nil, bytes.Repeat([]byte{'a'}, 1025), []byte("bad\nline\n"), []byte("!!!!")} {
		if _, err := Wrap(v, make([]byte, 32)); !errors.Is(err, ErrPublicKey) {
			t.Fatal("accepted public key")
		}
		if _, err := p.Unwrap(v); !errors.Is(err, ErrAnswer) {
			t.Fatal("accepted answer")
		}
	}
	for _, v := range []string{UnsettledLine, "berthy-key-wrap/1 unsettled\r\n"} {
		if _, err := p.Unwrap([]byte(v)); !errors.Is(err, ErrUnsettled) {
			t.Fatal("unsettled not reported")
		}
	}
}

func TestRejectsInvalidKeysAndAnswers(t *testing.T) {
	p, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&small.PublicKey)
	for _, input := range [][]byte{[]byte(base64.StdEncoding.EncodeToString(der)), append(p.PublicLine(), 'x'), append(bytes.TrimSpace(p.PublicLine()), '\r')} {
		if _, err := Wrap(input, make([]byte, 32)); !errors.Is(err, ErrPublicKey) {
			t.Fatal("invalid public key accepted")
		}
	}
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	answer, err := Wrap(other.PublicLine(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	wrongLabel, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &p.key.PublicKey, make([]byte, 32), []byte("wrong"))
	if err != nil {
		t.Fatal(err)
	}
	short, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &p.key.PublicKey, make([]byte, 31), label)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]byte{answer, []byte(answerPrefix + base64.StdEncoding.EncodeToString(wrongLabel)), []byte(answerPrefix + base64.StdEncoding.EncodeToString(short)), []byte(answerPrefix + base64.StdEncoding.EncodeToString(make([]byte, 383))), []byte(answerPrefix + base64.StdEncoding.EncodeToString(make([]byte, 385))), []byte("private wire content")} {
		_, err := p.Unwrap(input)
		if !errors.Is(err, ErrAnswer) || strings.Contains(err.Error(), string(input)) {
			t.Fatal("invalid answer accepted or disclosed")
		}
	}
}

func TestParsedPublicKeyWrapsAndRejectsMalformedDER(t *testing.T) {
	p, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicLine(p.PublicLine())
	if err != nil {
		t.Fatal("valid wrapping key refused", err)
	}
	key := bytes.Repeat([]byte{0xfb}, 32)
	answer, err := pub.Wrap(key)
	if err != nil {
		t.Fatal("wrap failed", err)
	}
	got, err := p.Unwrap(answer)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("round trip failed", err)
	}
	der, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(p.PublicLine())))
	if err != nil {
		t.Fatal(err)
	}
	even := rsa.PublicKey{N: new(big.Int).SetBit(new(big.Int).Set(p.key.N), 0, 0), E: 65537}
	evenDER, err := x509.MarshalPKIXPublicKey(&even)
	if err != nil {
		t.Fatal(err)
	}
	exponent := rsa.PublicKey{N: p.key.N, E: 3}
	exponentDER, err := x509.MarshalPKIXPublicKey(&exponent)
	if err != nil {
		t.Fatal(err)
	}
	ed, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, err := x509.MarshalPKIXPublicKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{"trailing DER": append(der, 0), "even modulus": evenDER, "exponent 3": exponentDER, "Ed25519": edDER} {
		t.Run(name, func(t *testing.T) {
			line := []byte(base64.StdEncoding.EncodeToString(input))
			if _, err := ParsePublicLine(line); !errors.Is(err, ErrPublicKey) {
				t.Fatal("invalid wrapping key accepted")
			}
			if _, err := Wrap(line, key); !errors.Is(err, ErrPublicKey) {
				t.Fatal("invalid wrapping key accepted by Wrap")
			}
		})
	}
}
