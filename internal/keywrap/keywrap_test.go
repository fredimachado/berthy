package keywrap

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
