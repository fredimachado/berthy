package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"berthy/internal/agent"
)

// agentWants are the owner and mode the agent insists on, as the probe's stat prints them.
const (
	wantPrivateDir = "berthy:berthy 700"
	wantPrivate    = "berthy:berthy 600"
	dockerSocket   = "/var/run/docker.sock"
)

// decodeAgentConfig reads the agent's configuration the way the agent does: unknown fields and trailing data are refused, and
// the result must validate. The errors never repeat a value of the file.
func decodeAgentConfig(b []byte) (agent.File, error) {
	c, err := agent.ParseFile(b)
	if errors.Is(err, agent.ErrOldLayout) {
		return c, errors.New("this server has the agent of an earlier Berthy release, which cannot be upgraded in place. Move it aside, do not delete it: sudo systemctl stop berthy && sudo mv /etc/berthy /etc/berthy.pre-projects && sudo mv /var/lib/berthy /var/lib/berthy.pre-projects && sudo chmod 0700 /var/lib/berthy.pre-projects, then run setup again. Its apps keep running and are yours to remove or adopt; its state and key stay in /var/lib/berthy.pre-projects.")
	}
	if err != nil {
		return c, fmt.Errorf("%s cannot be read as an agent configuration", configPath)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%s is not a valid agent configuration: %v", configPath, err)
	}
	return c, nil
}

// loadConfig reads the server's agent configuration once, into memory. It is never printed. The probe says whether there is
// one.
func (s *Session) loadConfig(ctx context.Context) (*agent.File, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return nil, err
	}
	if !f.AgentConfig {
		return nil, nil
	}
	if s.configRead {
		return s.existing, nil
	}
	res, err := s.R.Run(ctx, "cat "+configPath, nil)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("reading %s: %s", configPath, failureText(res))
	}
	c, err := decodeAgentConfig(res.Stdout)
	if err != nil {
		return nil, err
	}
	s.existing, s.existingRaw, s.configRead = &c, bytes.Clone(res.Stdout), true
	return s.existing, nil
}

// resolveListen settles the address the agent listens on: --listen on a first run, the configuration's afterwards. A different
// --listen than the configuration's is refused, because setup does not move the agent.
func (s *Session) resolveListen(ctx context.Context, f *Facts) error {
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return err
	}
	if cfg != nil {
		ip, port, err := ParseListen(cfg.Listen)
		if err != nil {
			return fmt.Errorf("%s: %v", configPath, err)
		}
		if s.Opts.ListenIP != nil && (!s.Opts.ListenIP.Equal(ip) || s.Opts.ListenPort != port) {
			return fmt.Errorf("the agent listens on %s; setup does not move it", cfg.Listen)
		}
		s.Opts.ListenIP, s.Opts.ListenPort = ip, port
		return nil
	}
	if s.Opts.ListenIP == nil {
		return errors.New("--listen ADDRESS:PORT is needed: this server has no agent configuration yet")
	}
	probe := agent.Config{Environment: s.Opts.Env, Listen: s.Opts.listen(), Tokens: []agent.Token{{Digest: strings.Repeat("0", 64), Scope: "admin"}}}
	if err := probe.Validate(); err != nil {
		return fmt.Errorf("invalid --listen %s: %v", s.Opts.listen(), err)
	}
	return nil
}

func inGroup(groups []string, name string) bool { return slices.Contains(groups, name) }

func userStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	var script []string
	var did []string
	if !f.UserExists {
		script = append(script, "useradd --system --user-group --home-dir "+dataDir+" --shell /usr/sbin/nologin berthy")
		did = append(did, "created the berthy user")
	}
	if !f.UserExists || !inGroup(f.UserGroups, "docker") {
		script = append(script, "usermod -aG docker berthy")
		did = append(did, "added it to the docker group")
	}
	if len(script) == 0 {
		return StatusOK, "berthy, in the docker group", nil
	}
	command := "set -e\n" + strings.Join(script, "\n") + "\n"
	if s.DryRun {
		s.Log.Would(command)
		return StatusWouldChange, "would " + wouldText(did), nil
	}
	if _, err := s.change(ctx, "creating the berthy user", command); err != nil {
		return "", "", err
	}
	f.UserExists = true
	f.UserGroups = append(f.UserGroups, "berthy", "docker")
	return StatusChanged, strings.Join(did, ", "), nil
}

// readKeyFile reads the key --key-file names: exactly 32 bytes.
func readKeyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read --key-file")
	}
	if len(b) != 32 {
		return nil, errors.New("--key-file must hold exactly 32 bytes")
	}
	return b, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keyStep makes sure the encryption key and the directories of the agent's state are in place. It never replaces a key.
func keyStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	stateDir, keyPath := s.paths()
	pf, err := probePaths(ctx, s.R, stateDir, keyPath)
	if err != nil {
		return "", "", err
	}
	var local []byte
	if s.Opts.KeyFile != "" {
		if local, err = readKeyFile(s.Opts.KeyFile); err != nil {
			return "", "", err
		}
		defer clear(local)
	}
	if pf.KeyExists {
		if !strings.HasPrefix(pf.KeyStat, "32 ") {
			return "", "", fmt.Errorf("%s is not 32 bytes; setup does not replace a key", keyPath)
		}
		if local != nil && sha256Hex(local) != pf.KeySHA {
			return "", "", errors.New("the server has a different encryption key; setup never replaces it")
		}
	} else if local == nil && pf.StateDB {
		return "", "", fmt.Errorf("%s has state but no key: pass --key-file with the saved key", stateDir)
	}

	// The directories, and the owner and mode of what is there.
	var fix, did []string
	if f.DataDirStat != wantPrivateDir {
		fix = append(fix, "install -d -o berthy -g berthy -m 0700 "+dataDir)
		did = append(did, "prepared "+dataDir)
	}
	if pf.StateDirStat != wantPrivateDir {
		fix = append(fix, "install -d -o berthy -g berthy -m 0700 "+shellQuote(stateDir))
		did = append(did, "prepared "+stateDir)
	}
	if pf.StateForeign {
		fix = append(fix, "chown -R berthy:berthy "+shellQuote(stateDir))
		did = append(did, "gave the state to berthy")
	}
	var stdin io.Reader
	switch {
	case pf.KeyExists:
		if !strings.HasSuffix(pf.KeyStat, " "+wantPrivate) {
			fix = append(fix, "chown berthy:berthy "+shellQuote(keyPath), "chmod 0600 "+shellQuote(keyPath))
			did = append(did, "fixed the owner and mode of the key")
		}
	case local != nil:
		fix = append(fix, keyUploadScript(keyPath))
		did = append(did, "installed the key from --key-file")
		stdin = bytes.NewReader(local)
	default:
		fix = append(fix, keyGenerateScript(keyPath))
		did = append(did, "generated an encryption key on the server")
	}
	if len(did) == 0 {
		return StatusOK, "the key and the state directory are in place", nil
	}
	script := "set -e\numask 077\n" + strings.Join(fix, "\n") + "\n"
	if s.DryRun {
		s.Log.Would(script)
		return StatusWouldChange, "would " + wouldText(did), nil
	}
	res, err := s.R.Run(ctx, script, stdin)
	if err != nil {
		return "", "", fmt.Errorf("preparing the key and the state directory: %w", err)
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf("preparing the key and the state directory: %s", failureText(res))
	}
	if local != nil && !pf.KeyExists {
		if got := firstField(res.Stdout); got != sha256Hex(local) {
			return "", "", errors.New("the key on the server does not match --key-file after the upload")
		}
	}
	return StatusChanged, strings.Join(did, ", "), nil
}

// wouldText turns what a step did into what it would do: "prepared X, generated Y" becomes "prepare X, generate Y".
func wouldText(did []string) string {
	out := make([]string, len(did))
	for i, d := range did {
		verb, rest, _ := strings.Cut(d, " ")
		switch verb {
		case "prepared":
			verb = "prepare"
		case "gave":
			verb = "give"
		case "fixed":
			verb = "fix"
		case "installed":
			verb = "install"
		case "generated":
			verb = "generate"
		case "created":
			verb = "create"
		case "added":
			verb = "add"
		}
		out[i] = verb + " " + rest
	}
	return strings.Join(out, ", ")
}

func firstField(b []byte) string {
	lines := nonEmptyLines(b)
	if len(lines) == 0 {
		return ""
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// keyGenerateScript makes the key on the server from its random source. The key never leaves the server.
func keyGenerateScript(keyPath string) string {
	dir := shellQuote(path.Dir(keyPath))
	return "install -d -o berthy -g berthy -m 0700 " + dir + "\n" +
		"tmp=$(mktemp " + shellQuote(path.Dir(keyPath)+"/.key.XXXXXX") + ")\n" +
		"trap 'rm -f \"$tmp\"' EXIT\n" +
		"head -c 32 /dev/urandom > \"$tmp\"\n" +
		"[ \"$(wc -c < \"$tmp\")\" -eq 32 ]\n" +
		"chown berthy:berthy \"$tmp\"\n" +
		"chmod 0600 \"$tmp\"\n" +
		"mv -f \"$tmp\" " + shellQuote(keyPath)
}

// keyUploadScript installs the key from standard input and prints its SHA-256.
func keyUploadScript(keyPath string) string {
	dir := shellQuote(path.Dir(keyPath))
	return "install -d -o berthy -g berthy -m 0700 " + dir + "\n" +
		"tmp=$(mktemp " + shellQuote(path.Dir(keyPath)+"/.key.XXXXXX") + ")\n" +
		"trap 'rm -f \"$tmp\"' EXIT\n" +
		"cat > \"$tmp\"\n" +
		"chown berthy:berthy \"$tmp\"\n" +
		"chmod 0600 \"$tmp\"\n" +
		"mv -f \"$tmp\" " + shellQuote(keyPath) + "\n" +
		"sha256sum " + shellQuote(keyPath)
}

// adminTokenName is the name of the first admin token.
const adminTokenName = "admin"

func tokenStep(ctx context.Context, s *Session) (string, string, error) {
	if _, err := s.loadConfig(ctx); err != nil {
		return "", "", err
	}
	more := s.hosted() != nil
	if more && !s.Opts.NewToken {
		return StatusOK, "the tokens of " + configPath + " are kept", nil
	}
	name := adminTokenName
	what := "an admin token"
	if more {
		name = adminTokenName + "-" + s.Opts.now().Format("20060102-150405")
		what = "another admin token"
	}
	if s.DryRun {
		s.dryToken = true
		return StatusWouldChange, "would create " + what, nil
	}
	token, digest, err := agent.NewToken(s.Opts.Env)
	if err != nil {
		return "", "", fmt.Errorf("creating the token: %w", err)
	}
	s.Token = token
	s.newToken = &agent.Token{Name: name, Digest: digest, Scope: "admin"}
	return StatusChanged, fmt.Sprintf("created %s named %s; it is shown once, at the end", what, name), nil
}

// hostedPaths names the state and key of one hosted environment on the Linux server.
func hostedPaths(project, env string) (string, string) {
	base := dataDir + "/environments/" + project + "/" + env
	return base + "/state", base + "/key"
}

// hosted returns the existing entry for this run, if there is one.
func (s *Session) hosted() *agent.Hosted {
	if s.existing != nil {
		for i := range s.existing.Environments {
			h := &s.existing.Environments[i]
			if h.Project == s.Opts.Project && h.Environment == s.Opts.Env {
				return h
			}
		}
	}
	return nil
}

func (s *Session) paths() (string, string) {
	if h := s.hosted(); h != nil {
		return h.StateDir, h.KeyFile
	}
	return hostedPaths(s.Opts.Project, s.Opts.Env)
}

// KeyFile is the selected environment's key on the server.
func (s *Session) KeyFile() string { _, key := s.paths(); return key }

// desiredFile keeps every other hosted environment and updates this one.
func (s *Session) desiredFile(f *Facts) (agent.File, error) {
	var file agent.File
	if s.existing != nil {
		file = *s.existing
		file.Environments = slices.Clone(s.existing.Environments)
	}
	if file.Listen == "" {
		file.Listen = s.Opts.listen()
	}
	if file.DockerSocket == "" {
		file.DockerSocket = dockerSocket
	}
	idx := -1
	for i, h := range file.Environments {
		if h.Project == s.Opts.Project && h.Environment == s.Opts.Env {
			idx = i
			break
		}
	}
	if idx < 0 {
		state, key := hostedPaths(s.Opts.Project, s.Opts.Env)
		file.Environments = append(file.Environments, agent.Hosted{Project: s.Opts.Project, Environment: s.Opts.Env, StateDir: state, KeyFile: key})
		idx = len(file.Environments) - 1
	}
	h := &file.Environments[idx]
	h.Tokens = slices.Clone(h.Tokens)
	h.Network = s.Opts.Network
	h.Protected = h.Protected || s.Opts.Protected
	if s.newToken != nil {
		h.Tokens = append(h.Tokens, *s.newToken)
	}
	check := file
	check.Environments = slices.Clone(file.Environments)
	if len(check.Environments[idx].Tokens) == 0 {
		// Validation precedes token generation. Pick a digest that no existing entry uses.
		used := map[string]bool{}
		for _, e := range check.Environments {
			for _, token := range e.Tokens {
				used[strings.ToLower(token.Digest)] = true
			}
		}
		for n := 0; ; n++ {
			digest := fmt.Sprintf("%064x", n)
			if !used[digest] {
				check.Environments[idx].Tokens = []agent.Token{{Digest: digest, Scope: "admin"}}
				break
			}
		}
	}
	if err := check.Validate(); err != nil {
		return file, fmt.Errorf("the agent configuration would not be valid: %v", err)
	}
	return file, nil
}

// marshalConfig is the file's content: indented JSON.
func marshalConfig(c agent.File) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// environmentRanges finds the original JSON object for each hosted environment.
func environmentRanges(b []byte) ([][2]int, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	var ranges [][2]int
	for d.More() {
		name, err := d.Token()
		if err != nil {
			return nil, err
		}
		if name != "environments" {
			var value json.RawMessage
			if err := d.Decode(&value); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		ranges = nil
		for d.More() {
			start := int(d.InputOffset())
			for start < len(b) && (b[start] == ',' || b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
				start++
			}
			var value json.RawMessage
			if err := d.Decode(&value); err != nil {
				return nil, err
			}
			ranges = append(ranges, [2]int{start, int(d.InputOffset())})
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
	}
	return ranges, nil
}

// keepOtherEntries retains the original bytes of entries this run does not own.
func (s *Session) keepOtherEntries(want []byte) ([]byte, error) {
	if s.existing == nil {
		return want, nil
	}
	oldRanges, err := environmentRanges(s.existingRaw)
	if err != nil {
		return nil, err
	}
	newRanges, err := environmentRanges(want)
	if err != nil {
		return nil, err
	}
	if len(oldRanges) != len(s.existing.Environments) || len(newRanges) < len(oldRanges) {
		return nil, errors.New("cannot preserve hosted environment entries")
	}
	for i := len(oldRanges) - 1; i >= 0; i-- {
		h := s.existing.Environments[i]
		if h.Project == s.Opts.Project && h.Environment == s.Opts.Env {
			continue
		}
		old, next := oldRanges[i], newRanges[i]
		out := make([]byte, 0, len(want)-(next[1]-next[0])+(old[1]-old[0]))
		out = append(out, want[:next[0]]...)
		out = append(out, s.existingRaw[old[0]:old[1]]...)
		want = append(out, want[next[1]:]...)
	}
	return want, nil
}

// configWriteScript writes the configuration that arrives on the script's standard input to agent.json.new: owned by berthy and
// private, for the service step to put in place.
func configWriteScript() string {
	return "set -e\numask 077\n" +
		"install -d -o root -g berthy -m 0750 " + agentConfigDir + "\n" +
		"cat > " + configPath + ".new\n" +
		"chown berthy:berthy " + configPath + ".new\n" +
		"chmod 0600 " + configPath + ".new\n"
}

// configFixScript gives the live configuration the owner and mode the agent insists on.
func configFixScript() string {
	return "chown berthy:berthy " + configPath + " && chmod 0600 " + configPath
}

// configStep writes the agent's configuration next to the live one, only when its content changes; the service step puts it in
// place. It starts from the server's own configuration and keeps everything setup does not own.
func configStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	if _, err := s.loadConfig(ctx); err != nil {
		return "", "", err
	}
	cfg, err := s.desiredFile(f)
	if err != nil {
		return "", "", err
	}
	want, err := marshalConfig(cfg)
	if err != nil {
		return "", "", err
	}
	want, err = s.keepOtherEntries(want)
	if err != nil {
		return "", "", fmt.Errorf("preserving %s: %w", configPath, err)
	}
	addsToken := s.newToken != nil || (s.DryRun && (s.hosted() == nil || s.Opts.NewToken))
	tokens := len(cfg.Environments)
	summary := fmt.Sprintf("%s (listen %s, %d %s)", configPath, cfg.Listen, tokens, plural(tokens, "environment", "environments"))
	changed := s.existing == nil || addsToken
	if !changed {
		have, err := marshalConfig(*s.existing)
		if err != nil {
			return "", "", err
		}
		canonicalWant, err := marshalConfig(cfg)
		if err != nil {
			return "", "", err
		}
		changed = !bytes.Equal(have, canonicalWant)
	}
	if !changed {
		if f.AgentConfigStat == wantPrivate {
			return StatusOK, summary, nil
		}
		fix := configFixScript()
		if s.DryRun {
			s.Log.Would(fix)
			return StatusWouldChange, "would fix the owner and mode of " + configPath, nil
		}
		if _, err := s.change(ctx, "fixing "+configPath, fix); err != nil {
			return "", "", err
		}
		return StatusChanged, "fixed the owner and mode of " + configPath, nil
	}
	verb := "update"
	if s.existing == nil {
		verb = "create"
	}
	script := configWriteScript()
	if s.DryRun {
		s.staged.Config = true
		s.Log.Would(script)
		return StatusWouldChange, "would " + verb + " " + summary, nil
	}
	res, err := s.R.Run(ctx, script, bytes.NewReader(want))
	if err != nil {
		return "", "", fmt.Errorf("writing %s: %w", configPath, err)
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf("writing %s: %s", configPath, failureText(res))
	}
	s.staged.Config = true
	return StatusChanged, verb + "d " + summary, nil
}
