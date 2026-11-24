package cli

import (
	"berthy/internal/keywrap"
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestKeyWrapHandsOverOnlyWrapped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("server reader is Linux")
	}
	p, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0xfb}, 32)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	if code := keyWrap(path, bytes.NewReader(p.PublicLine()), &out, &diag); code != 0 {
		t.Fatal("helper failed", code)
	}
	forms := [][]byte{key, []byte(hex.EncodeToString(key)), []byte(base64.StdEncoding.EncodeToString(key)), []byte(base64.RawStdEncoding.EncodeToString(key)), []byte(base64.URLEncoding.EncodeToString(key)), []byte(base64.RawURLEncoding.EncodeToString(key))}
	if bytes.Equal(forms[2], forms[4]) {
		t.Fatal("fixture does not distinguish base64 alphabets")
	}
	for _, stream := range [][]byte{out.Bytes(), diag.Bytes()} {
		for _, form := range forms {
			if bytes.Contains(stream, form) {
				t.Fatal("key form disclosed in helper output")
			}
		}
	}
	got, err := p.Unwrap(out.Bytes())
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("invalid handover", err)
	}
	if err := os.WriteFile(path+".next", nil, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diag.Reset()
	if code := keyWrap(path, bytes.NewReader(p.PublicLine()), &out, &diag); code != 3 || out.String() != keywrap.UnsettledLine {
		t.Fatal("unsettled not reported")
	}
}
func TestAgentKeyWrapDispatches(t *testing.T) {
	var out, diag bytes.Buffer
	code := Run([]string{"agent", "key-wrap"}, &out, &diag)
	if code != 2 || !bytes.Contains(diag.Bytes(), []byte("key-wrap")) {
		t.Fatal("helper was not dispatched", code, diag.String())
	}
}

func TestKeyWrapRejectsUncheckedInputs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("server reader is Linux")
	}
	p, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		size       int
		mode       os.FileMode
	}{{"symlink", link, 32, 0600}, {"short", filepath.Join(dir, "short"), 31, 0600}, {"long", filepath.Join(dir, "long"), 33, 0600}, {"public", filepath.Join(dir, "public"), 32, 0644}, {"directory", dir, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name != "symlink" && tc.name != "directory" {
				if err := os.WriteFile(tc.path, make([]byte, tc.size), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			var out, diag bytes.Buffer
			if keyWrap(tc.path, bytes.NewReader(p.PublicLine()), &out, &diag) == 0 || out.Len() != 0 {
				t.Fatal("unchecked key accepted")
			}
		})
	}
	var out, diag bytes.Buffer
	keyWrap(filepath.Join(dir, "missing"), bytes.NewReader([]byte("bad")), &out, &diag)
	if !bytes.Contains(diag.Bytes(), []byte("wrapping key")) || out.Len() != 0 {
		t.Fatal("public key was not checked first")
	}
}

func TestAManifestAliasNamedKeyWrapStillWorks(t *testing.T) {
	if err := checkAliases(map[string]string{"key-wrap": "ps"}); err != nil {
		t.Fatal(err)
	}
	srv, requests := aliasAgent(t)
	t.Setenv("BERTHY_CONFIG", ingressCLIConfig(t, srv.URL))
	dir := writeAliasDir(t, aliasManifest+"  key-wrap: ps\n")
	t.Chdir(dir)
	var out, diag bytes.Buffer
	if code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &diag); code != 0 {
		t.Fatal("manifest rejected", diag.String())
	}
	out.Reset()
	diag.Reset()
	if code := Run([]string{"key-wrap", "-e", "dev"}, &out, &diag); code != 0 {
		t.Fatal("alias failed", diag.String())
	}
	got := requests()
	if len(got) != 1 || got[0].method != "GET" {
		t.Fatal("alias did not reach agent")
	}
}

func TestAgentKeyWrapDispatchWithSyntheticInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = w.Write([]byte("synthetic invalid wrapping key\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	previous := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = previous }()
	var out, diag bytes.Buffer
	code := Run([]string{"agent", "key-wrap", filepath.Join(t.TempDir(), "key")}, &out, &diag)
	want := "key-wrap runs on the server\n"
	if runtime.GOOS == "linux" {
		u, err := user.Lookup("berthy")
		if err == nil && u.Uid == strconv.Itoa(os.Geteuid()) {
			t.Skip("test already runs as the agent user")
		}
		want = "key-wrap: must run as the agent user\n"
	}
	if code != 1 || out.Len() != 0 || diag.String() != want {
		t.Fatal("dispatch did not enforce the platform and agent-user restriction")
	}
}

func TestKeyWrapRejectsEvenModulusBeforeReadingKey(t *testing.T) {
	p, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(p.PublicLine())))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey := parsed.(*rsa.PublicKey)
	rsaKey.N = new(big.Int).SetBit(rsaKey.N, 0, 0)
	der, err = x509.MarshalPKIXPublicKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	code := keyWrap(filepath.Join(t.TempDir(), "missing"), bytes.NewReader([]byte(base64.StdEncoding.EncodeToString(der))), &out, &diag)
	if code != 1 || out.Len() != 0 || diag.String() != "key-wrap: invalid wrapping key\n" {
		t.Fatal("invalid wrapping key was not rejected before reading")
	}
}
