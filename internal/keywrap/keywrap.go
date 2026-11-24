// Package keywrap carries an encryption key encrypted for one setup run.
package keywrap

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
)

const Bits = 3072
const UnsettledLine = "berthy-key-wrap/1 unsettled\n"
const answerPrefix = "berthy-key-wrap/1 ok "

var (
	ErrPublicKey = errors.New("not a wrapping key")
	ErrAnswer    = errors.New("not a wrapped key")
	ErrUnsettled = errors.New("the server's key rotation is not finished")
)
var label = []byte("berthy key copy v1")

type Private struct{ key *rsa.PrivateKey }

func Generate() (*Private, error) {
	k, err := rsa.GenerateKey(rand.Reader, Bits)
	if err != nil {
		return nil, err
	}
	return &Private{key: k}, nil
}
func (p *Private) PublicLine() []byte {
	der, _ := x509.MarshalPKIXPublicKey(&p.key.PublicKey)
	return append([]byte(base64.StdEncoding.EncodeToString(der)), '\n')
}
func line(b []byte) ([]byte, bool) {
	if len(b) > 1024 {
		return nil, false
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		b = bytes.TrimSuffix(b, []byte("\n"))
		b = bytes.TrimSuffix(b, []byte("\r"))
	}
	if bytes.ContainsAny(b, "\r\n") {
		return nil, false
	}
	return b, true
}
func Wrap(publicLine, key []byte) ([]byte, error) {
	b, ok := line(publicLine)
	if !ok || len(key) != 32 {
		return nil, ErrPublicKey
	}
	der, err := base64.StdEncoding.Strict().DecodeString(string(b))
	if err != nil {
		return nil, ErrPublicKey
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, ErrPublicKey
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != Bits || pub.E != 65537 {
		return nil, ErrPublicKey
	}
	canonical, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil || !bytes.Equal(der, canonical) {
		return nil, ErrPublicKey
	}
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, key, label)
	if err != nil {
		return nil, ErrPublicKey
	}
	return []byte(answerPrefix + base64.StdEncoding.EncodeToString(cipher) + "\n"), nil
}
func (p *Private) Unwrap(answer []byte) ([]byte, error) {
	b, ok := line(answer)
	if !ok {
		return nil, ErrAnswer
	}
	if string(b) == "berthy-key-wrap/1 unsettled" {
		return nil, ErrUnsettled
	}
	if !bytes.HasPrefix(b, []byte(answerPrefix)) {
		return nil, ErrAnswer
	}
	cipher, err := base64.StdEncoding.Strict().DecodeString(string(b[len(answerPrefix):]))
	if err != nil || len(cipher) != Bits/8 {
		return nil, ErrAnswer
	}
	key, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, p.key, cipher, label)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, ErrAnswer
	}
	return key, nil
}
