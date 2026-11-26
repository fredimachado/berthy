package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/manifest"

	"github.com/charmbracelet/x/term"
)

// maxImportLine is the longest line an import reads. A request holds at most api.MaxRequestBody bytes, so no line can be longer.
const maxImportLine = api.MaxRequestBody

// stdinTerminal says whether standard input is a terminal. It is a variable so tests can stand in for the terminal.
var stdinTerminal = func() bool { return term.IsTerminal(os.Stdin.Fd()) }

// parseSecretLines reads the NAME=value lines of an import. The value is everything after the first =, with one trailing
// carriage return removed. Blank lines and lines that start with # are skipped. A line without =, or whose name is not a
// secret name, is refused by its line number. The line itself is never echoed, since it may hold a value.
func parseSecretLines(r io.Reader) ([]api.SecretEntry, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxImportLine)
	var entries []api.SecretEntry
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected NAME=value", line)
		}
		if err := api.CheckSecretName(name); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		entries = append(entries, api.SecretEntry{Name: name, Value: value})
	}
	if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
		return nil, fmt.Errorf("line %d: longer than %d bytes", line+1, maxImportLine)
	} else if err != nil {
		return nil, errors.New("secret input unavailable")
	}
	return entries, nil
}

// readSecretImport reads the import from standard input and checks it before anything is sent: the entries must pass
// api.CheckSecretImport, and the request must fit in one request body. An import that does not fit is split by the person
// who runs it.
func readSecretImport(dry bool) (api.SecretImport, error) {
	entries, err := parseSecretLines(os.Stdin)
	if err != nil {
		return api.SecretImport{}, err
	}
	if err := api.CheckSecretImport(entries); err != nil {
		return api.SecretImport{}, err
	}
	body := api.SecretImport{Secrets: entries, DryRun: dry}
	if payload, _ := json.Marshal(body); len(payload) > api.MaxRequestBody {
		return api.SecretImport{}, errors.New("the import is too large: split it")
	}
	return body, nil
}

// printSecretImport says what an import did: the names a dry run would create and replace, or the names that were created and
// replaced. No value is printed; the answer holds none.
func printSecretImport(out, errout io.Writer, data []byte, dry bool) int {
	if dry {
		var plan api.SecretImportPlan
		if json.Unmarshal(data, &plan) != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		printNames(out, "would create", plan.Create)
		printNames(out, "would replace", plan.Replace)
		return 0
	}
	var res api.SecretImportResult
	if json.Unmarshal(data, &res) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	printNames(out, "created", res.Created)
	printNames(out, "replaced", res.Replaced)
	return 0
}

// printNames prints one line, "label: name, name", or nothing when there are no names.
func printNames(out io.Writer, label string, names []string) {
	if len(names) == 0 {
		return
	}
	safe := make([]string, 0, len(names))
	for _, n := range names {
		safe = append(safe, terminalSafe(n))
	}
	fmt.Fprintf(out, "%s: %s\n", label, strings.Join(safe, ", "))
}

// runSecretList answers secret list with --manifest-dir. It prints the secrets the agent holds, as the list does, then the names
// the manifest needs that are not held, each with what needs it. With --json it prints one object: the list under secrets, and
// the missing names under missing. The manifest must load, since a list that cannot say what the manifest needs is not the
// check the person asked for.
func runSecretList(f commandFlags, u *url.URL, prefix, token string, out, errout io.Writer) int {
	m, err := manifest.Load(f.dir, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	held, ok := storedSecrets(ctx, agentclient.NewHTTPClient(), agentURL(u, prefix, "secrets"), token, errout, !f.json)
	if !ok {
		return 1
	}
	needs := unmetSecrets(m, held)
	missing := make([]manifest.SecretNeed, 0, len(needs))
	for _, n := range needs {
		missing = append(missing, n.SecretNeed)
	}
	if f.json {
		b, err := json.Marshal(struct {
			Secrets []storedSecret        `json:"secrets"`
			Missing []manifest.SecretNeed `json:"missing"`
		}{held, missing})
		if err != nil {
			return 1
		}
		return printResponse(out, errout, b, true)
	}
	safeHeld := make([]storedSecret, len(held))
	for i, item := range held {
		safeHeld[i] = storedSecret{Name: terminalSafe(item.Name), Registry: terminalSafe(item.Registry)}
	}
	b, _ := json.Marshal(safeHeld)
	if code := printResponseShape(out, errout, b, false, shapeSecrets); code != 0 {
		return code
	}
	if len(missing) == 0 {
		fmt.Fprintln(out, "every secret the manifest needs is stored")
	}
	for _, n := range needs {
		name := terminalSafe(n.Name)
		switch n.Kind {
		case unmetRegistry:
			if n.Stored == "" {
				fmt.Fprintf(out, "missing: %s (stored without registry %s; needed by registry %s)\n", name, terminalSafe(n.Registry), terminalSafe(n.Registry))
			} else {
				fmt.Fprintf(out, "missing: %s (stored for registry %s; needed by registry %s)\n", name, terminalSafe(n.Stored), terminalSafe(n.Registry))
			}
		case unmetMixed:
			fmt.Fprintf(out, "conflict: %s is needed by %s: give them separate secret names\n", name, mixedReaders(n))
		case unmetNotPlain:
			fmt.Fprintf(out, "missing: %s (stored as the password of %s; needed by %s)\n", name, terminalSafe(n.Stored), terminalSafe(strings.Join(n.NeededBy, ", ")))
		case unmetShared:
			fmt.Fprintf(out, "conflict: %s is the password_secret of %s: give each registry its own secret name\n", name, safeRegistryList(n.Registries))
		default:
			fmt.Fprintf(out, "missing: %s (needed by %s)\n", name, terminalSafe(strings.Join(n.NeededBy, ", ")))
		}
	}
	return 0
}

type unmetKind int

const (
	unmetMissing unmetKind = iota
	unmetRegistry
	unmetNotPlain
	unmetShared
	unmetMixed
)

type unmetNeed struct {
	manifest.SecretNeed
	Kind       unmetKind
	Registry   string
	Stored     string
	Registries []string
}

func registriesOf(m *manifest.Manifest, name string) []string {
	var regs []string
	for host, reg := range m.Registries {
		if reg.PasswordSecret == name {
			regs = append(regs, host)
		}
	}
	sort.Strings(regs)
	return regs
}
func safeRegistryList(regs []string) string {
	safe := make([]string, len(regs))
	for i, r := range regs {
		safe[i] = terminalSafe(r)
	}
	return strings.Join(safe, " and ")
}
func mixedReaders(n unmetNeed) string {
	parts := []string{"registry " + terminalSafe(n.Registry)}
	for _, who := range n.NeededBy {
		if !strings.HasPrefix(who, "registry ") {
			parts = append(parts, terminalSafe(who))
		}
	}
	return strings.Join(parts, " and ")
}

func unmetSecrets(m *manifest.Manifest, held []storedSecret) []unmetNeed {
	stored := make(map[string]storedSecret, len(held))
	for _, s := range held {
		stored[s.Name] = s
	}
	needs := []unmetNeed{}
	for _, n := range m.SecretNeeds() {
		regs := registriesOf(m, n.Name)
		s, ok := stored[n.Name]
		u := unmetNeed{SecretNeed: n}
		switch {
		case len(regs) > 1:
			u.Kind = unmetShared
			u.Registries = regs
		case len(regs) == 1 && len(n.NeededBy) > 1:
			u.Kind = unmetMixed
			u.Registry = regs[0]
		case !ok:
			u.Kind = unmetMissing
			if len(regs) == 1 {
				u.Registry = regs[0]
			}
		case len(regs) == 1 && s.Registry != regs[0]:
			u.Kind = unmetRegistry
			u.Registry = regs[0]
			u.Stored = s.Registry
		case len(regs) == 0 && s.Registry != "":
			u.Kind = unmetNotPlain
			u.Stored = s.Registry
		default:
			continue
		}
		needs = append(needs, u)
	}
	return needs
}
