package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/scope"
)

// RemovalSteps verifies, unhosts and cleans up the selected environment.
func RemovalSteps(purge bool) []Step {
	return []Step{
		{"connect", connectStep},
		{"check", func(ctx context.Context, s *Session) (string, string, error) {
			return prepareRemoval(ctx, s, purge)
		}},
		{"config", stageRemoval},
		{"service", activateRemoval},
		{"state", removeStateStep},
		{"network", removeNetworkStep},
		{"cli_config", removeCLIConfigStep},
	}
}

// The install probe asks Docker about the manifest network. Removal uses only the socket and network in agent.json.
func loadRemovalConfig(ctx context.Context, s *Session) (*agent.File, error) {
	script := "# inspect removal configuration\nset -eu\n" + removalStat + `
owner=$(id -u berthy)
group=$(id -g berthy)
dir=/etc/berthy
[ "$(realpath -e -- "$dir")" = "$dir" ]
[ "$(stat -c '%u %g %a %F' -- "$dir")" = "0 $group 750 directory" ]
stat_entry /etc/berthy/agent.json
case "$entry" in *" $owner 600 1 regular file") ;; *) exit 1;; esac
for file in /etc/berthy/agent.json.new /etc/berthy/agent.json.prev; do
  stat_entry "$file"
  case "$entry" in absent|*" $owner 600 1 regular file"|*" 0 600 1 regular file") ;; *) exit 1;; esac
done
` + removalReadLimit(configPath) + "printf 'present\\n'\n"
	res, err := s.R.Run(ctx, script, nil)
	if err != nil || res.Code != 0 || string(res.Stdout) != "present\n" || len(res.Stderr) != 0 {
		return nil, errors.New("could not verify the live agent configuration and its staging paths")
	}
	s.F = &Facts{AgentConfig: true}
	return s.loadConfig(ctx)
}

func prepareRemoval(ctx context.Context, s *Session, purge bool) (string, string, error) {
	id := scope.ID{Project: s.Opts.Project, Environment: s.Opts.Env}
	if !scope.ValidProject(id.Project) || !scope.ValidEnvironment(id.Environment) || id.CheckLength() != nil {
		return "", "", errors.New("removal requires a valid project and environment")
	}
	cfg, err := loadRemovalConfig(ctx, s)
	if err != nil {
		return "", "", err
	}
	s.Opts.ListenIP, s.Opts.ListenPort, err = ParseListen(cfg.Listen)
	if err != nil {
		return "", "", err
	}
	pending, err := readRemovalRecord(ctx, s.R, id)
	if err != nil {
		return "", "", err
	}
	generation := configDigest(s.existingRaw)
	refreshRecord := false
	if pending != nil {
		switch {
		case generation == pending.CandidateSHA:
			if purge != (pending.Archive == "") {
				return "", "", errors.New("this removal was started with a different purge choice")
			}
			return resumeLiveRemoval(ctx, s, cfg, pending)
		case pending.Activated:
			return "", "", fmt.Errorf("the agent configuration changed after removal was activated. Keep %s. Retry with the verified post-removal configuration, or finish cleanup manually using the recorded identities before deleting the record", removalRecordPath(id))
		case generation != pending.OriginalSHA:
			if s.hosted() == nil {
				return "", "", errors.New("the agent configuration changed since this removal started")
			}
			refreshRecord = true
		case purge != (pending.Archive == ""):
			return "", "", errors.New("this removal was started with a different purge choice")
		}
	}
	if guard, err := settledRemovalGuard(ctx, s.R); err != nil {
		if guard != "pending" || pending == nil || !orphanedRemovalGuard(ctx, s.R, pending.OriginalSHA) {
			return "", "", err
		}
	}
	candidate, target, err := withoutHosted(s.existingRaw, id)
	if err != nil {
		return "", "", err
	}
	rec := removal{ID: id, StateDir: target.StateDir, KeyFile: target.KeyFile, Network: target.Network,
		OriginalSHA: generation, CandidateSHA: configDigest(candidate)}
	if !purge {
		if pending == nil || refreshRecord {
			rec.Archive = rec.StateDir + ".removed-" + s.Opts.now().Format(removalTime)
		} else {
			rec.Archive = pending.Archive
		}
	}
	if pending != nil && (!sameRemovalTarget(rec, *pending) || !refreshRecord && rec.CandidateSHA != pending.CandidateSHA) {
		return "", "", retireRemovalRecord(id, "the removal record does not match the selected configuration")
	}
	rec, err = inspectRemovalPaths(ctx, s.R, rec, *cfg)
	if err != nil {
		return "", "", err
	}
	if pending != nil && !sameRemovalFiles(rec, *pending) {
		return "", "", retireRemovalRecord(id, "the removal file identities changed since this removal started")
	}
	if err := requireNoAppliedApps(ctx, s.R, id, rec.StateDir); err != nil {
		return "", "", err
	}
	if rec.Network != "" {
		docker, err := removalDockerCommand(ctx, s.R, cfg.DockerSocket, rec.Network)
		if err != nil {
			return "", "", err
		}
		network, err := inspectRemovalNetwork(ctx, s.R, docker, rec.Network, nil)
		if err != nil {
			return "", "", err
		}
		rec.NetworkID = network.id
		s.removalNetwork = &network
	}
	if pending != nil && rec.NetworkID != pending.NetworkID {
		return "", "", retireRemovalRecord(id, "the configured Docker network changed since this removal started")
	}
	if local, err := agentclient.ConfigPath(s.Opts.CLIConfig); err == nil {
		if _, err := ReadCLIConfig(local); err != nil {
			return "", "", err
		}
	}
	if (pending == nil || refreshRecord) && !s.DryRun {
		if err := writeRemovalRecord(ctx, s.R, rec); err != nil {
			return "", "", err
		}
	}
	s.removal, s.removalCandidate = &rec, candidate
	s.hostedCount = len(cfg.Environments) - 1
	return StatusOK, id.String() + " has no applied apps", nil
}

func sameRemovalTarget(a, b removal) bool {
	return a.ID == b.ID && a.StateDir == b.StateDir && a.KeyFile == b.KeyFile && a.Network == b.Network
}

func retireRemovalRecord(id scope.ID, reason string) error {
	return fmt.Errorf("%s. On the server, verify the pair is still hosted and identify which files and network belong to it. If you intend to remove the current resources, move the record aside from %s to a secure backup before retrying", reason, removalRecordPath(id))
}

func sameRemovalFiles(a, b removal) bool {
	same := func(x, y *fileIdentity) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.StateIdentity == b.StateIdentity && a.KeyIdentity == b.KeyIdentity &&
		same(a.NextIdentity, b.NextIdentity) && same(a.PrevIdentity, b.PrevIdentity)
}

func settledRemovalGuard(ctx context.Context, r Runner) (string, error) {
	raw, err := readRemovalFile(ctx, r, "removal guard state", guardState, "0")
	if err != nil || raw == nil {
		return "", err
	}
	switch string(raw) {
	case "pending\n", "recovering\n":
		return strings.TrimSuffix(string(raw), "\n"), errors.New("the activation guard is still running. Retry removal after it finishes")
	case "confirmed\n", "kept\n", "restored\n", "restored-down\n":
		return strings.TrimSuffix(string(raw), "\n"), nil
	default:
		return "", errors.New("could not verify the activation guard's decision")
	}
}

// The swap holds this same lock from the pending write through timer registration and the configuration swap.
func orphanedRemovalGuard(ctx context.Context, r Runner, original string) bool {
	script := "# check for orphaned removal guard\nset -eu\n" + removalStat +
		"exec 9<" + shellQuote(guardLock) + "\nflock -n 9\n" +
		"state=" + shellQuote(guardState) + "\nstat_entry \"$state\"\n" +
		"case \"$entry\" in *\" 0 600 1 regular file\") ;; *) exit 1;; esac\n" +
		"[ \"$(cat -- \"$state\")\" = pending ]\n" +
		"sum=$(sha256sum -- " + shellQuote(configPath) + ")\n" +
		"[ \"${sum%% *}\" = " + shellQuote(original) + " ]\n" +
		"units=$(systemctl list-units --all --plain --no-legend --no-pager " + guardUnit + ".timer " + guardUnit + ".service)\n" +
		"[ -z \"$units\" ]\n" +
		"jobs=$(systemctl list-jobs --plain --no-legend --no-pager)\n" +
		"printf '%s\\n' \"$jobs\" | awk '$2 == \"" + guardUnit + ".timer\" || $2 == \"" + guardUnit + ".service\" { found = 1 } END { exit found }'\n"
	res, err := r.Run(ctx, script, nil)
	return err == nil && res.Code == 0 && len(res.Stdout) == 0 && len(res.Stderr) == 0
}

func resumeLiveRemoval(ctx context.Context, s *Session, cfg *agent.File, rec *removal) (string, string, error) {
	if !rec.Activated {
		guard, err := settledRemovalGuard(ctx, s.R)
		if err != nil {
			return "", "", err
		}
		raw, err := readRemovalFile(ctx, s.R, "previous removal configuration", configPath+".prev", "$(id -u berthy)")
		if err != nil || raw == nil || configDigest(raw) != rec.OriginalSHA {
			return "", "", errors.New("could not verify the previous configuration for this removal")
		}
		candidate, target, err := withoutHosted(raw, rec.ID)
		if err != nil || configDigest(candidate) != rec.CandidateSHA ||
			target.StateDir != rec.StateDir || target.KeyFile != rec.KeyFile || target.Network != rec.Network {
			return "", "", errors.New("the previous configuration does not match this removal")
		}
		if guard == "" {
			return "", "", errors.New("the activation decision is missing. Restore the verified previous configuration before retrying: " +
				"sudo cp -p -- " + configPath + ".prev " + configPath + ".new && sudo mv -fT -- " + configPath + ".new " + configPath + " && sudo systemctl restart berthy")
		}
		if guard != "confirmed" && guard != "kept" {
			return "", "", errors.New("the activation guard did not confirm this removal")
		}
		cfg, err := agent.ParseFile(raw)
		if err != nil {
			return "", "", errors.New("could not verify the previous configuration for this removal")
		}
		observed, err := inspectRemovalPaths(ctx, s.R, *rec, cfg)
		if err != nil {
			return "", "", err
		}
		if !sameRemovalFiles(*rec, observed) {
			return "", "", errors.New("the removal file identities changed since this removal started")
		}
	}
	for _, peer := range cfg.Environments {
		if peer.Project == rec.ID.Project && peer.Environment == rec.ID.Environment {
			return "", "", errors.New("the removal record still names a hosted environment")
		}
	}
	s.removal = rec
	return StatusOK, rec.ID.String() + " is no longer hosted", nil
}

func stageRemoval(ctx context.Context, s *Session) (string, string, error) {
	if s.removalCandidate == nil {
		return StatusOK, configPath + " already excludes " + s.removal.ID.String(), nil
	}
	if err := s.stageConfig(ctx, s.removalCandidate); err != nil {
		return "", "", errors.New("could not stage " + configPath)
	}
	if s.DryRun {
		return StatusWouldChange, "would remove " + s.removal.ID.String() + " from " + configPath, nil
	}
	return StatusChanged, "staged " + configPath + " without " + s.removal.ID.String(), nil
}

func activateRemoval(ctx context.Context, s *Session) (string, string, error) {
	if s.removal.Activated {
		return StatusOK, "the agent already confirmed the removal", nil
	}
	staged := Staged{Config: true}
	wait := s.Opts.healthTimeout(s.hostedCount)
	if s.DryRun {
		if s.removalCandidate == nil {
			return StatusOK, "the activation guard already confirmed the removal", nil
		}
		s.Log.Would(prepareProbeScript(staged))
		for _, script := range activateScripts(staged, s.Opts.listen(), wait) {
			s.Log.Would(script)
		}
		return StatusWouldChange, "would restart berthy.service with the reduced configuration", nil
	}
	if s.removalCandidate != nil {
		if _, err := s.change(ctx, "preparing the setup probe", prepareProbeScript(staged)); err != nil {
			return "", "", activationFailure(err, ConfigPrevious)
		}
		if err := ctx.Err(); err != nil {
			return "", "", activationFailure(fmt.Errorf("preparing the setup probe: %w", err), ConfigPrevious)
		}
		if err := Activate(ctx, s.R, s.Opts.listen(), staged, wait); err != nil {
			return "", "", err
		}
	}
	rec := *s.removal
	rec.Activated = true
	if err := writeRemovalRecord(ctx, s.R, rec); err != nil {
		return "", "", err
	}
	s.removal.Activated = true
	return StatusChanged, "confirmed the agent is running with the reduced configuration", nil
}
