package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"berthy/internal/manifest"

	"github.com/charmbracelet/x/term"
)

// maxSecretValue is the largest value a secret can have, in bytes.
const maxSecretValue = 65536

// cloudflareTokenTemplate is the start of Cloudflare's account API token form with its permissions filled in. Cloudflare
// reads the rest of the link as the token's permission groups and name.
const cloudflareTokenTemplate = "https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys="

// cloudflareTunnelWritePermission is Cloudflare's permission group ID for "Cloudflare Tunnel Write".
const cloudflareTunnelWritePermission = "c07321b023e944ff818fec44d8203567"

// cloudflareDNSPermission is Cloudflare's documented short key for the DNS permission.
const cloudflareDNSPermission = "dns"

// secretGuide is what Berthy knows about a secret before it asks for its value at a terminal.
type secretGuide struct {
	// intro is printed before the prompt: what the secret is for and, when Berthy knows one, the link that creates it.
	intro []string
}

// cloudflarePermission is one permission group of a Cloudflare token link.
type cloudflarePermission struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// guideFor works out the guide from the secret's name, the --registry host and the manifest.
// m is nil when no manifest was loaded.
func guideFor(m *manifest.Manifest, env, name, registry string) secretGuide {
	token := "berthy-" + env
	if m != nil {
		token = "berthy-" + m.Project + "-" + env
	}
	switch {
	case registry == "ghcr.io":
		link := "https://github.com/settings/tokens/new?" + url.Values{"scopes": {"read:packages"}, "description": {token}}.Encode()
		return secretGuide{intro: []string{
			name + " is the password Berthy uses to pull images from ghcr.io.",
			"Create a GitHub token (classic) that can only read packages:",
			"  " + link,
		}}
	case registry != "":
		return secretGuide{intro: []string{name + " is the password Berthy uses to pull images from " + registry + "."}}
	case m != nil && m.Ingress != nil && m.Ingress.Cloudflare != nil && m.Ingress.Cloudflare.APITokenSecret == name:
		c := m.Ingress.Cloudflare
		return cloudflareGuide(name, token, c.DNS == nil || *c.DNS)
	}
	return secretGuide{}
}

// cloudflareGuide is the guide for the managed ingress API token: a link to Cloudflare's account token form with the
// permissions the token needs filled in. The DNS permission is in the link only when the manifest keeps DNS on.
func cloudflareGuide(name, token string, dns bool) secretGuide {
	perms := []cloudflarePermission{{Key: cloudflareTunnelWritePermission, Type: "edit"}}
	if dns {
		perms = append(perms, cloudflarePermission{Key: cloudflareDNSPermission, Type: "edit"})
	}
	keys, _ := json.Marshal(perms) // a list of plain strings always marshals
	link := cloudflareTokenTemplate + url.QueryEscape(string(keys)) + "&name=" + url.QueryEscape(token)
	intro := []string{name + " is the Cloudflare API token for managed ingress."}
	if dns {
		intro = append(intro,
			"Create it with the permissions filled in (Account > Cloudflare Tunnel > Edit, Zone > DNS > Edit):",
			"  "+link,
			"Limit it to the zones that hold your hostnames.")
	} else {
		intro = append(intro,
			"Create it with the permission filled in (Account > Cloudflare Tunnel > Edit):",
			"  "+link)
	}
	return secretGuide{intro: intro}
}

// hiddenInput returns a function that reads one line from standard input without showing it.
// ok is false when standard input is not a terminal. It is a variable so tests can stand in for the terminal.
var hiddenInput = func() (read func() ([]byte, error), ok bool) {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		return nil, false
	}
	return func() ([]byte, error) { return readHidden(fd) }, true
}

// readHidden reads one line from the terminal fd with echo off. term.ReadPassword turns echo back on only when it returns,
// so a Ctrl-C while it waits restores the terminal itself and exits with 130.
func readHidden(fd uintptr) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-interrupt:
			// The process exits next, so a failed restore has nowhere to be reported.
			_ = term.Restore(fd, state)
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	return term.ReadPassword(fd)
}

// secretValue returns the value `secret set` stores. From a pipe it reads standard input to its end, as before.
// At a terminal it prints the guide, asks, and reads the value hidden. code is non-zero when there is no value to store.
func secretValue(f commandFlags, errout io.Writer) (value string, code int) {
	read, interactive := hiddenInput()
	if !interactive {
		data, e := io.ReadAll(io.LimitReader(os.Stdin, maxSecretValue+1))
		if e != nil || len(data) > maxSecretValue {
			fmt.Fprintln(errout, "secret input unavailable or too large")
			return "", 1
		}
		return strings.TrimSuffix(string(data), "\n"), 0
	}
	name := f.pos[1]
	m, err := manifest.Load(f.dir, f.env)
	if err != nil {
		m = nil
		// A directory with no berthy.yml is the common case and says nothing; a manifest that is there but does not load is said.
		if _, statErr := os.Stat(filepath.Join(f.dir, "berthy.yml")); statErr == nil {
			fmt.Fprintf(errout, "manifest not loaded: %v\n", err)
		}
	}
	for _, line := range guideFor(m, f.env, name, f.registry).intro {
		fmt.Fprintln(errout, line)
	}
	fmt.Fprintf(errout, "Value of %s (hidden): ", name)
	line, err := read()
	fmt.Fprintln(errout)
	if err != nil || len(line) > maxSecretValue {
		fmt.Fprintln(errout, "secret input unavailable or too large")
		return "", 1
	}
	if len(line) == 0 {
		fmt.Fprintln(errout, "no value entered")
		return "", 1
	}
	return string(line), 0
}
