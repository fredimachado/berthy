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
	"runtime"
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

// onlyLine is the one line of wire as the format reads it: at most 1,024 bytes, with one optional LF or CRLF at the end
// and no other line break.
func onlyLine(wire []byte) ([]byte, bool) {
	if len(wire) > 1024 {
		return nil, false
	}
	s := wire
	if bytes.HasSuffix(s, []byte("\r\n")) {
		s = s[:len(s)-2]
	} else if bytes.HasSuffix(s, []byte("\n")) {
		s = s[:len(s)-1]
	}
	return s, !bytes.ContainsAny(s, "\r\n")
}

// wantPublicLine says whether wire is a canonical 3072-bit RSA public key with exponent 65537 and an odd modulus.
func wantPublicLine(wire []byte) bool {
	s, ok := onlyLine(wire)
	if !ok {
		return false
	}
	der, err := base64.StdEncoding.Strict().DecodeString(string(s))
	if err != nil {
		return false
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return false
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 3072 || pub.N.Bit(0) != 1 || pub.E != 65537 {
		return false
	}
	canonical, err := x509.MarshalPKIXPublicKey(pub)
	return err == nil && bytes.Equal(canonical, der)
}

// The line endings a valid line is given by the high bits of the mode of FuzzKeywrap.
const (
	endBare = iota
	endLF
	endCRLF
	endEmbedded // a line break in the middle of the line, and an LF at the end
)

// withEnding puts the ending on a line that ends in one LF.
func withEnding(line []byte, ending uint8) []byte {
	body := bytes.TrimSuffix(line, []byte("\n"))
	switch ending {
	case endBare:
		return body
	case endLF:
		return append(bytes.Clone(body), '\n')
	case endCRLF:
		return append(bytes.Clone(body), '\r', '\n')
	}
	half := len(body) / 2
	return append(append(append(bytes.Clone(body[:half]), '\n'), body[half:]...), '\n')
}

// The operations of FuzzKeywrap, in the low three bits of its mode. Each builds its input from the worker's own key, so a
// corpus entry replays the same way whichever worker made it.
const (
	opRaw       = iota // wire as given
	opPublic           // the worker's public line with the ending
	opAnswer           // a wrapped synthetic key with the ending
	opPublicBit        // the worker's public line with one byte of it changed
	opAnswerBit        // a wrapped synthetic key with one byte of the answer changed
	opUnsettled        // the unsettled answer with the ending
	opLong             // the worker's public line padded beyond 1,024 bytes
)

func FuzzKeywrap(f *testing.F) {
	priv, err := Generate()
	if err != nil {
		f.Fatal(err)
	}
	publicLine := priv.PublicLine()
	pub, err := ParsePublicLine(publicLine)
	if err != nil {
		f.Fatal(err)
	}

	// Raw seeds: valid and invalid wrapping keys, answers and lines.
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatal(err)
	}
	smallDER, _ := x509.MarshalPKIXPublicKey(&small.PublicKey)
	der, _ := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(publicLine)))
	even, _ := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: new(big.Int).SetBit(new(big.Int).Set(priv.key.N), 0, 0), E: 65537})
	exponent, _ := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: priv.key.N, E: 3})
	edKey, _, _ := ed25519.GenerateKey(rand.Reader)
	edDER, _ := x509.MarshalPKIXPublicKey(edKey)
	answer, err := pub.Wrap(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		f.Fatal(err)
	}
	short, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &priv.key.PublicKey, make([]byte, 31), label)
	if err != nil {
		f.Fatal(err)
	}
	wrongLabel, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &priv.key.PublicKey, make([]byte, 32), []byte("wrong"))
	if err != nil {
		f.Fatal(err)
	}
	for _, wire := range [][]byte{
		nil, []byte("\n"), []byte("\r\n"), []byte("bad\nline\n"), []byte("!!!!"), bytes.Repeat([]byte{'a'}, 1024), bytes.Repeat([]byte{'a'}, 1025),
		publicLine, answer, []byte(UnsettledLine), []byte("berthy-key-wrap/1 unsettled\r\n"), []byte("berthy-key-wrap/1 unsettled"),
		[]byte(answerPrefix), []byte(answerPrefix + "!!!!\n"), []byte("private wire content"),
		append(bytes.Clone(publicLine), 'x'), append(bytes.Clone(bytes.TrimSpace(publicLine)), '\r'),
		[]byte(base64.StdEncoding.EncodeToString(smallDER)), []byte(base64.StdEncoding.EncodeToString(append(bytes.Clone(der), 0))),
		[]byte(base64.StdEncoding.EncodeToString(even)), []byte(base64.StdEncoding.EncodeToString(exponent)), []byte(base64.StdEncoding.EncodeToString(edDER)),
		[]byte(base64.RawStdEncoding.EncodeToString(der)), []byte(base64.URLEncoding.EncodeToString(der)),
		[]byte(answerPrefix + base64.StdEncoding.EncodeToString(short)), []byte(answerPrefix + base64.StdEncoding.EncodeToString(wrongLabel)),
		[]byte(answerPrefix + base64.StdEncoding.EncodeToString(make([]byte, 383))), []byte(answerPrefix + base64.StdEncoding.EncodeToString(make([]byte, 385))),
	} {
		f.Add(uint8(opRaw), wire)
	}
	for ending := uint8(0); ending < 4; ending++ {
		for _, op := range []uint8{opPublic, opAnswer, opUnsettled} {
			f.Add(op|ending<<3, []byte("synthetic key material"))
		}
	}
	f.Add(uint8(opPublicBit), []byte{0, 0, 1})
	f.Add(uint8(opPublicBit), []byte{200, 1, 0x20})
	f.Add(uint8(opAnswerBit), []byte{0, 0, 1})
	f.Add(uint8(opAnswerBit), []byte{40, 0, 0xff})
	f.Add(uint8(opLong), []byte("A"))

	f.Fuzz(func(t *testing.T, mode uint8, wire []byte) {
		ending := mode >> 3 & 3
		syntheticKey := sha256.Sum256(wire)
		key := syntheticKey[:]
		var public, answer []byte
		switch mode & 7 {
		case opPublic:
			public = withEnding(publicLine, ending)
		case opPublicBit, opLong:
			public = bytes.Clone(publicLine)
		case opAnswer, opAnswerBit:
			wrapped, err := pub.Wrap(key)
			if err != nil {
				t.Fatalf("wrap a synthetic key: %v", err)
			}
			answer = wrapped
			if mode&7 == opAnswer {
				answer = withEnding(wrapped, ending)
			}
		case opUnsettled:
			answer = withEnding([]byte(UnsettledLine), ending)
		default:
			public, answer = wire, wire
		}
		if mode&7 == opPublicBit || mode&7 == opAnswerBit {
			target := &public
			if mode&7 == opAnswerBit {
				target = &answer
			}
			at, flip := 0, byte(1)
			if len(wire) >= 2 {
				at = int(wire[0]) | int(wire[1])<<8
			}
			if len(wire) >= 3 && wire[2] != 0 {
				flip = wire[2]
			}
			at %= len(*target)
			(*target)[at] ^= flip
		}
		if mode&7 == opLong {
			public = append(public[:len(public)-1], bytes.Repeat([]byte{'A'}, 1025)...)
		}

		if public != nil {
			p, err := ParsePublicLine(public)
			want := wantPublicLine(public)
			if (err == nil) != want {
				t.Fatalf("mode %d: ParsePublicLine accepted %v (%d bytes), the canonical-key check says %v", mode, err == nil, len(public), want)
			}
			if err != nil && !errors.Is(err, ErrPublicKey) {
				t.Fatalf("mode %d: a refused public line returned another error", mode)
			}
			if err == nil {
				wrapped, err := p.Wrap(key)
				if err != nil {
					t.Fatalf("mode %d: wrap with an accepted key: %v", mode, err)
				}
				if mode&7 == opPublic {
					if got, err := priv.Unwrap(wrapped); err != nil || !bytes.Equal(got, key) {
						t.Fatalf("mode %d: round trip failed: %v", mode, err)
					}
				}
			}
			if mode&7 == opPublic && ending != endEmbedded && !want {
				t.Fatalf("mode %d: the worker's own public line was refused", mode)
			}
			if mode&7 == opLong && err == nil {
				t.Fatal("a public line over 1,024 bytes was accepted")
			}
			if _, err := Wrap(public, key); (err == nil) != want {
				t.Fatalf("mode %d: Wrap and ParsePublicLine disagree", mode)
			}
		}
		if answer != nil {
			got, err := priv.Unwrap(answer)
			s, single := onlyLine(answer)
			unsettled := single && string(s) == "berthy-key-wrap/1 unsettled"
			switch {
			case err == nil:
				if len(got) != 32 || got == nil {
					t.Fatalf("mode %d: an accepted answer returned %d bytes", mode, len(got))
				}
			case errors.Is(err, ErrUnsettled):
				if !unsettled || got != nil {
					t.Fatalf("mode %d: unsettled reported for a line that is not (%d bytes)", mode, len(answer))
				}
			case errors.Is(err, ErrAnswer):
				if unsettled || got != nil {
					t.Fatalf("mode %d: a refusal of the unsettled line or with a key", mode)
				}
			default:
				t.Fatalf("mode %d: a refused answer returned another error", mode)
			}
			switch mode & 7 {
			case opAnswer:
				if ending == endEmbedded {
					if err == nil {
						t.Fatal("an answer with a line break inside was accepted")
					}
				} else if err != nil || !bytes.Equal(got, key) {
					t.Fatalf("mode %d: a wrapped key did not come back: %v", mode, err)
				}
			case opAnswerBit:
				if err == nil && bytes.Equal(got, key) {
					t.Fatal("an answer with one byte changed returned the original key")
				}
			case opUnsettled:
				if ending == endEmbedded {
					if !errors.Is(err, ErrAnswer) {
						t.Fatalf("mode %d: unsettled with a line break inside: %v", mode, err)
					}
				} else if !errors.Is(err, ErrUnsettled) {
					t.Fatalf("mode %d: unsettled line: %v", mode, err)
				}
			}
		}
	})
}

// A line over 1,024 bytes is refused before anything is decoded: the allocation does not follow the input.
func TestOverlongLineIsRefusedBeforeDecoding(t *testing.T) {
	p, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	publicWire := bytes.Repeat([]byte{'A'}, 1<<20)
	answerWire := append([]byte(answerPrefix), publicWire...)
	for name, refuse := range map[string]func() error{
		"public line": func() error { _, err := ParsePublicLine(publicWire); return err },
		"answer":      func() error { _, err := p.Unwrap(answerWire); return err },
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := refuse()
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("%s: a megabyte accepted", name)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<10 {
			t.Errorf("%s: %d bytes allocated for a refused megabyte", name, allocated)
		}
	}
}
