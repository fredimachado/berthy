package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"berthy/internal/backup"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func TestDeployRefusesAServiceNameAnotherEnvironmentHolds(t *testing.T) {
	ctx := context.Background()
	claim := state.Claim{Kind: state.ClaimService, Name: "shop-prod-eu-api-web"}
	c := &Controller{Environment: "prod-eu", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }}
	req := Request{App: "api", Version: "sha256:" + strings.Repeat("a", 64), RequestID: "one", Manifest: manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {}}}}}}
	want := "name shop-prod-eu-api-web is taken by another project or environment on this server"
	if _, err := c.Plan(ctx, Actor{ID: "test"}, req); err == nil || err.Error() != want {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := c.Admit(ctx, Actor{ID: "test"}, req); err == nil || err.Error() != want {
		t.Fatalf("admission: %v", err)
	}
	c.Store = openStore(t)
	e := &positiveEngine{}
	c.Engine = e
	c.Reserve = func(context.Context, []state.Claim) ([]state.Claim, error) { return nil, Refusal(want) }
	app := req.Manifest.Apps["api"]
	one := 1
	app.Roles["web"] = manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	req.Manifest.Apps["api"] = app
	out, _ := c.runDeploy(ctx, state.Operation{}, Actor{ID: "test"}, req, false)
	if out.Err != want || e.spec != nil {
		t.Fatalf("run: %+v; service: %s", out, e.spec)
	}
}

func TestComposeDeployRefusesAProjectNameAnotherEnvironmentHolds(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	m := composeApp(false)
	app := m.Apps["db"]
	app.ProjectName = "shared"
	m.Apps["db"] = app
	claim := state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	req := composeReq("project", composeFileOne, m)
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "Compose project name is taken") {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := c.Admit(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "Compose project name is taken") {
		t.Fatalf("admit: %v", err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("compose up ran")
	}
}

func TestComposeDeployRefusesAVolumeNameTheFileSetsThatAnotherEnvironmentHolds(t *testing.T) {
	for _, external := range []string{"false", "true"} {
		t.Run(external, func(t *testing.T) {
			runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"volumes":{"pgdata":{"name":"pgdata","external":` + external + `}}}`, hashes: hashOne}
			c, _ := composeController(t, runner, newComposeDocker())
			claim := state.Claim{Kind: state.ClaimVolume, Name: "pgdata"}
			c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
			_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, composeReq("volume", composeFileOne, composeApp(false)))
			if err == nil || !strings.Contains(err.Error(), "Compose volumes.pgdata name is taken") {
				t.Fatalf("plan: %v", err)
			}
			if runner.upRuns() != 0 {
				t.Fatal("compose up ran")
			}
		})
	}
}

func TestComposeDeployRefusesANetworkAnotherEnvironmentHolds(t *testing.T) {
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"networks":{"shared":{"name":"shared-net","external":true}}}`, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	claim := state.Claim{Kind: state.ClaimNetwork, Name: "shared-net"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, composeReq("network", composeFileOne, composeApp(false)))
	if err == nil || !strings.Contains(err.Error(), "Compose networks.shared name is taken") {
		t.Fatalf("plan: %v", err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("compose up ran")
	}
}

func TestComposeRunReservesAgainBeforeUp(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	c.Reserve = func(context.Context, []state.Claim) ([]state.Claim, error) {
		return nil, Refusal("name shared is taken by another project or environment on this server")
	}
	outcome := c.runCompose(context.Background(), state.Operation{}, Actor{ID: "admin", Admin: true}, composeReq("run", composeFileOne, composeApp(false)), false)
	if outcome.Err != "name shared is taken by another project or environment on this server" || runner.upRuns() != 0 {
		t.Fatalf("outcome: %+v; up calls: %d", outcome, runner.upRuns())
	}
}

func TestComposeRejectsUninspectableNamesBeforeReservation(t *testing.T) {
	for _, kind := range []string{"networks", "volumes"} {
		for _, length := range []int{128, 129} {
			t.Run(kind+strconv.Itoa(length), func(t *testing.T) {
				name := strings.Repeat("a", length)
				runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"` + kind + `":{"item":{"name":"` + name + `"}}}`, hashes: hashOne}
				c, _ := composeController(t, runner, newComposeDocker())
				calls := 0
				c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) { calls++; return claims, nil }
				req := composeReq("names", composeFileOne, composeApp(false))
				if length == 129 {
					if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), kind+".item") || strings.Contains(err.Error(), name) {
						t.Fatalf("dry run: %v", err)
					}
				}
				out := c.runCompose(context.Background(), state.Operation{}, Actor{ID: "admin", Admin: true}, req, false)
				if length == 129 {
					if calls != 0 || !strings.Contains(out.Err, kind+".item") || strings.Contains(out.Err, name) || !strings.Contains(out.Err, "128") || !PublicMessage(out.Err) {
						t.Fatalf("outcome=%+v reservations=%d", out, calls)
					}
				} else if calls == 0 {
					t.Fatalf("valid name was not reserved: %+v", out)
				}
			})
		}
	}
}

func TestInvalidComposeNameMessageIsPublicAndStrict(t *testing.T) {
	for _, key := range []string{"backend", "db-data", "_private", "db.1"} {
		msg := string(invalidComposeName("networks", key))
		if !PublicMessage(msg) {
			t.Fatalf("not public: %q", msg)
		}
		if PublicMessage(msg + " extra") {
			t.Fatalf("accepted extra text: %q", msg)
		}
	}
	for _, msg := range []string{
		"Compose networks name is not a valid Docker name of at most 128 characters",
		"Compose networks.bad/name name is not a valid Docker name of at most 128 characters",
		"Compose network name \"secret\" is invalid; Docker names must be at most 128 characters and start with a letter or digit, followed by letters, digits, '.', '_' or '-'",
	} {
		if PublicMessage(msg) {
			t.Fatalf("unexpected public message: %q", msg)
		}
	}
}

type interpolatedNetworkRunner struct{ *fakeCompose }

func (r *interpolatedNetworkRunner) Run(ctx context.Context, call engine.ComposeRun) ([]byte, []byte, error) {
	if len(call.Args) > 1 && call.Args[0] == "config" && call.Args[1] == "--format" {
		env, err := os.ReadFile(filepath.Join(call.Dir, ".env"))
		if err != nil {
			return nil, nil, err
		}
		var value string
		for _, line := range strings.Split(string(env), "\n") {
			if strings.HasPrefix(line, "DATABASE_URL=") {
				value = strings.Trim(strings.TrimPrefix(line, "DATABASE_URL="), "'")
			}
		}
		config, err := json.Marshal(map[string]any{"services": map[string]any{"database": map[string]string{"image": "alpine"}}, "networks": map[string]any{"backend": map[string]string{"name": value}}})
		return config, nil, err
	}
	return r.fakeCompose.Run(ctx, call)
}

func TestInvalidInterpolatedComposeNetworkNeverDisclosesSecret(t *testing.T) {
	ctx := context.Background()
	const secret = "postgres://user:password@database/app"
	runner := &fakeCompose{hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	c.Compose = &interpolatedNetworkRunner{runner}
	if err := s.PutSecret(ctx, state.Actor{ID: "admin", Name: "admin"}, state.SecretInfo{Name: "DB_URL"}, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env = manifest.Env{Secret: []manifest.SecretVar{{Var: "DATABASE_URL", Secret: "DB_URL"}}}
	m.Apps["db"] = app
	req := composeReq("secret-network", "services:\n  database:\n    image: alpine\nnetworks:\n  backend:\n    name: ${DATABASE_URL}\n", m)
	t.Run("dry-run", func(t *testing.T) {
		_, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req)
		if err == nil {
			t.Fatal("invalid name accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Error("dry-run disclosed the secret")
		}
		if !strings.Contains(err.Error(), "networks.backend") {
			t.Error("dry-run did not name networks.backend")
		}
	})
	t.Run("operation", func(t *testing.T) {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "secret-network", ActorID: "admin", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		out := c.runCompose(ctx, op, Actor{ID: "admin", Admin: true}, req, false)
		if err = s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
		recorded, err := s.Operation(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(recorded)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) {
			t.Error("operation record disclosed the secret")
		}
		if !strings.Contains(string(body), "networks.backend") {
			t.Error("operation record did not name networks.backend")
		}
	})
}

func TestImageRunReservesAgainBeforeDockerChanges(t *testing.T) {
	c, _ := composeController(t, &fakeCompose{}, newComposeDocker())
	c.Reserve = func(context.Context, []state.Claim) ([]state.Claim, error) {
		return nil, Refusal("name shared is taken by another project or environment on this server")
	}
	one := 1
	req := Request{App: "api", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "shop", Network: "overlay", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	outcome, _ := c.runDeploy(context.Background(), state.Operation{}, Actor{ID: "actor"}, req, false)
	if outcome.Err != "name shared is taken by another project or environment on this server" {
		t.Fatalf("outcome: %+v", outcome)
	}
}

func TestRestoreIntoRefusesAVolumeAnotherEnvironmentHolds(t *testing.T) {
	c := restoreController(t, &fakeRestorer{})
	claim := state.Claim{Kind: state.ClaimVolume, Name: "restored-1"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	req := restoreRequest("conflict")
	if _, err := c.Plan(context.Background(), restoreAdmin, req); err == nil || !strings.Contains(err.Error(), "name restored-1 is taken") {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := c.Admit(context.Background(), restoreAdmin, req); err == nil || !strings.Contains(err.Error(), "name restored-1 is taken") {
		t.Fatalf("admit: %v", err)
	}
}

func TestTwoRestoresIntoOneNameHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	r := &fakeRestorer{}
	a, b := restoreController(t, r), restoreController(t, r)
	var mu sync.Mutex
	var barrier sync.WaitGroup
	barrier.Add(2)
	held := false
	for _, c := range []*Controller{a, b} {
		c := c
		c.Reserve = func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
			barrier.Done()
			barrier.Wait()
			mu.Lock()
			defer mu.Unlock()
			if held {
				return nil, Refusal("name restored-1 is taken by another project or environment on this server")
			}
			held = true
			return c.Store.AddClaimsNew(ctx, claims)
		}
	}
	ops := make([]state.Operation, 2)
	for i, c := range []*Controller{a, b} {
		var err error
		ops[i], _, err = c.Store.Admit(ctx, state.Admission{RequestID: "r", ActorID: "admin", Kind: "restore", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan state.Outcome, 2)
	for i, c := range []*Controller{a, b} {
		i, c := i, c
		go func() { <-start; results <- c.runRestore(ctx, ops[i], restoreAdmin, restoreRequest("r")) }()
	}
	close(start)
	first := <-results
	second := <-results
	loser, winner := first, second
	if first.State == "succeeded" {
		loser, winner = second, first
	}
	if loser.State != "failed" || !strings.Contains(loser.Err, "name restored-1 is taken") || winner.State != "succeeded" {
		t.Fatalf("outcomes: %+v, %+v", loser, winner)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.restores) != 1 {
		t.Fatalf("restore calls: %v", r.restores)
	}
}

func TestApplyRefusesANetworkAnotherEnvironmentHolds(t *testing.T) {
	c, _ := composeController(t, &fakeCompose{}, newComposeDocker())
	m := composeApp(false)
	m.Network = "shared"
	claim := state.Claim{Kind: state.ClaimNetwork, Name: "shared"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	for _, dry := range []bool{true, false} {
		_, err := c.ApplySettings(context.Background(), Actor{ID: "admin"}, ApplyRequest{Manifest: m, DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "name shared is taken") {
			t.Fatalf("dry=%v: %v", dry, err)
		}
	}
}

func TestRemoveNeverBringsDownAProjectOrDeletesAVolumeAnotherEnvironmentHolds(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	docker := newComposeDocker()
	docker.volumes["pgdata"] = true
	c, _ := composeController(t, runner, docker)
	claims := map[state.Claim]bool{{Kind: state.ClaimComposeProject, Name: "shared"}: true, {Kind: state.ClaimVolume, Name: "pgdata"}: true}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: claims}, nil }
	if err := c.composeDown(context.Background(), "shared", true); err == nil || !strings.Contains(err.Error(), "name shared is taken") {
		t.Fatalf("compose down: %v", err)
	}
	if err := c.removeVolume(context.Background(), "pgdata", func(string) {}); err == nil || !strings.Contains(err.Error(), "name pgdata is taken") {
		t.Fatalf("volume delete: %v", err)
	}
	for _, call := range runner.runs {
		if call.Args[0] == "down" {
			t.Fatal("compose down ran")
		}
	}
	if !docker.volumes["pgdata"] {
		t.Fatal("volume removed")
	}
}

func TestInvalidNetworkRefusalIsPublic(t *testing.T) {
	m := composeApp(false)
	m.Network = strings.Repeat("a", 64)
	err := m.Validate()
	if err == nil || err.Error() != "invalid project or network" {
		t.Fatalf("validation: %v", err)
	}
	if !PublicMessage(err.Error()) {
		t.Fatalf("not public: %v", err)
	}
}

func TestRemovalReleasesAServiceConfirmedGone(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimService, Name: "gone"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: &positiveEngine{}}
	if err := c.deleteBoundService(ctx, state.Intent{Name: "gone", ServiceID: "id"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claims(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("claims: %v, %v", got, err)
	}
}

func TestRemoveWithoutVolumesKeepsTheVolumesClaim(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimVolume, Name: "data"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	docker := &volumeDocker{loggedEngine: &loggedEngine{calls: &callLog{}}, volumes: map[string]string{"data": ""}}
	c := Controller{Store: s, Engine: docker}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "admin", Kind: "remove", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	journal := 0
	out := c.removeApp(ctx, op.ID, Request{App: "app"}, &removal{plan: RemovePlan{Kind: "image", KeptVolumes: []string{"data"}}}, func(string) {}, &journal)
	if out.State != "succeeded" {
		t.Fatalf("remove: %+v", out)
	}
	if err := s.Finish(ctx, op.ID, out); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claims(ctx)
	if err != nil || len(got) != 1 || got[0] != claim {
		t.Fatalf("claims: %v, %v", got, err)
	}
	other := Controller{Foreign: func(ctx context.Context) (Foreign, error) {
		cs, err := s.Claims(ctx)
		f := Foreign{Claims: map[state.Claim]bool{}}
		for _, c := range cs {
			f.Claims[c] = true
		}
		return f, err
	}}
	if err := other.notForeign(ctx, claim); err == nil {
		t.Fatal("another environment could take the retained volume")
	}
}

func TestRemoveWithVolumesReleasesTheVolumesClaim(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimVolume, Name: "data"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	docker := &volumeDocker{loggedEngine: &loggedEngine{calls: &callLog{}}, volumes: map[string]string{"data": ""}}
	c := Controller{Store: s, Engine: docker}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "admin", Kind: "remove", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	journal := 0
	out := c.removeApp(ctx, op.ID, Request{App: "app", Volumes: true}, &removal{plan: RemovePlan{Kind: "image", Volumes: []string{"data"}}}, func(string) {}, &journal)
	if out.State != "succeeded" {
		t.Fatalf("remove: %+v", out)
	}
	if err := s.Finish(ctx, op.ID, out); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claims(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("claims: %v, %v", got, err)
	}
}

func TestRemoveReleasesTheClaimOfAServiceAlreadyGone(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimService, Name: "shop-test-api-web"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	m := manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {}}}}}
	raw, _ := json.Marshal(m)
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "deploy", ActorID: "admin", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Digest: "sha256:" + strings.Repeat("a", 64), Manifest: raw, Bindings: []state.Binding{{App: "api", Role: "web", Name: claim.Name, ServiceID: "id"}}}}); err != nil {
		t.Fatal(err)
	}
	op, _, err = s.Admit(ctx, state.Admission{RequestID: "remove", ActorID: "admin", Kind: "remove", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: &positiveEngine{}, Environment: "test"}
	out := c.runRemove(ctx, op, Actor{ID: "admin", Admin: true}, Request{App: "api"})
	if out.State != "succeeded" {
		t.Fatalf("remove: %+v", out)
	}
	if err := s.Finish(ctx, op.ID, out); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claims(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("claims: %v, %v", got, err)
	}
}

type namesByComposeDirectory struct {
	*fakeCompose
	firstDir string
	count    int
}

func (f *namesByComposeDirectory) Run(ctx context.Context, r engine.ComposeRun) ([]byte, []byte, error) {
	if len(r.Args) > 1 && r.Args[0] == "config" && r.Args[1] != "--hash" {
		if f.firstDir == "" {
			f.firstDir = r.Dir
		}
		f.count++
		name := "v" + strings.TrimPrefix(filepath.Base(r.Dir), ".") + strconv.Itoa(f.count)
		return []byte(`{"services":{"database":{"image":"alpine"}},"volumes":{"data":{"name":"` + name + `"}}}`), nil, nil
	}
	return f.fakeCompose.Run(ctx, r)
}

func TestComposeRunReservesTheNamesOfItsStagedFiles(t *testing.T) {
	runner := &namesByComposeDirectory{fakeCompose: &fakeCompose{hashes: hashOne}}
	c, _ := composeController(t, runner.fakeCompose, newComposeDocker())
	c.Compose = runner
	var reserved []state.Claim
	c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
		reserved = claims
		return nil, Refusal("compose deploy failed")
	}
	c.runCompose(context.Background(), state.Operation{}, Actor{ID: "admin", Admin: true}, composeReq("r", composeFileOne, composeApp(false)), false)
	want := "v" + strings.TrimPrefix(filepath.Base(runner.firstDir), ".") + "1"
	if !slices.Contains(reserved, state.Claim{Kind: state.ClaimVolume, Name: want}) {
		t.Fatalf("stage %s reserved %v", runner.firstDir, reserved)
	}
}

func TestAdoptByAnExplicitServiceNameRefusesAServiceOfAnotherEnvironment(t *testing.T) {
	claim := state.Claim{Kind: state.ClaimService, Name: "existing"}
	c := Controller{Environment: "test", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }}
	req := Request{Adopt: true, App: "web", Manifest: manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {Service: "existing"}}}}}}
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err == nil || !strings.Contains(err.Error(), "name existing is taken") {
		t.Fatalf("plan: %v", err)
	}
}

func TestTheAgentRefusesALongServiceNameWithTheManifestsText(t *testing.T) {
	ctx := context.Background()
	app := strings.Repeat("a", 45)
	m := manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{app: {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {}}}}}
	req := Request{App: app, Manifest: m}
	c := Controller{Environment: "production", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{}}, nil }}
	want := m.ServiceNameError(c.Environment, app, "web")
	if want == "" {
		t.Fatal("test name fits")
	}
	if _, err := c.Plan(ctx, Actor{ID: "admin"}, req); err == nil || err.Error() != want || !PublicMessage(err.Error()) {
		t.Fatalf("plan: %v, want %s", err, want)
	}
	s := openStore(t)
	c.Store = s
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "a", ActorID: "admin", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Digest: "sha256:" + strings.Repeat("a", 64), Manifest: raw, Bindings: []state.Binding{{App: app, Role: "web", Name: "short", ServiceID: "id"}}}}); err != nil {
		t.Fatal(err)
	}
	names, err := c.effectiveNames(ctx, req)
	if err != nil || !slices.Contains(names, state.Claim{Kind: state.ClaimService, Name: "short"}) {
		t.Fatalf("bound role: %v, %v", names, err)
	}
}

func TestRollbackRefusesANameAnotherEnvironmentTookSince(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	m := manifest.Manifest{Project: "shop", Network: "overlay", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {}}}}}
	raw, _ := json.Marshal(m)
	for _, id := range []string{"first", "second"} {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "admin", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Digest: "sha256:" + strings.Repeat("a", 64), Manifest: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	claim := state.Claim{Kind: state.ClaimService, Name: "shop-test-api-web"}
	c := Controller{Store: s, Environment: "test", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }}
	req := Request{Kind: "rollback", App: "api", RequestID: "rollback"}
	if _, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "name shop-test-api-web is taken") {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := c.Admit(ctx, Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "name shop-test-api-web is taken") {
		t.Fatalf("admit: %v", err)
	}
}

func TestTheNameCheckLeavesThePlanByteIdentical(t *testing.T) {
	ctx := context.Background()
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	req := composeReq("plan", composeFileOne, composeApp(false))
	req.DryRun = true
	before, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	a, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{}}, nil }
	after, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("plan changed:\n%s\n%s", a, b)
	}
}

type failedFirstDeployEngine struct{ *positiveEngine }

func (e *failedFirstDeployEngine) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{}, errors.New("image unavailable")
}
func (e *failedFirstDeployEngine) VolumeExists(context.Context, string) (bool, error) {
	return false, nil
}

func TestAFailedFirstDeployKeepsItsNames(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	c := Controller{rolloutHold: zeroTestRolloutHold(), Store: s, Engine: &failedFirstDeployEngine{positiveEngine: &positiveEngine{}}, Project: "shop", Environment: "test"}
	c.Reserve = func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
		return s.AddClaimsNew(ctx, claims)
	}
	one := 1
	req := Request{App: "api", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "shop", Network: "overlay", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}, Volumes: []string{"data:/data"}}}}}}}
	out, _ := c.runDeploy(ctx, state.Operation{}, Actor{ID: "actor"}, req, false)
	if out.State != "failed" {
		t.Fatalf("deploy: %+v", out)
	}
	got, err := s.Claims(ctx)
	want := []state.Claim{{Kind: state.ClaimNetwork, Name: "overlay"}, {Kind: state.ClaimService, Name: "shop-test-api-web"}, {Kind: state.ClaimVolume, Name: "shop-test_data"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("claims: %v, %v", got, err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "destroy", ActorID: "admin", Kind: "destroy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	result := c.runDestroy(ctx, op, Actor{ID: "admin", Admin: true}, Request{Confirm: c.id().String()})
	if result.State != "succeeded" {
		t.Fatalf("destroy: %+v", result)
	}
	held, err := s.Claims(ctx)
	if err != nil || !slices.Equal(held, []state.Claim{{Kind: state.ClaimNetwork, Name: "overlay"}}) {
		t.Fatalf("after destroy: %v, %v", held, err)
	}

}

func TestFailedRestoreBeforeCreatingVolumeKeepsClaim(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(strconv.FormatBool(exists), func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			docker := newComposeDocker()
			docker.volumes["restored-1"] = exists
			c := Controller{Store: s, Engine: docker, Environment: "test", Restorer: &fakeRestorer{err: errors.New("failed")}}
			c.Reserve = func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
				return s.AddClaimsNew(ctx, claims)
			}
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "admin", Kind: "restore", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			out := c.runRestore(ctx, op, restoreAdmin, restoreRequest("r"))
			if out.State != "failed" {
				t.Fatalf("restore: %+v", out)
			}
			if err := s.Finish(ctx, op.ID, out); err != nil {
				t.Fatal(err)
			}
			got, err := s.Claims(ctx)
			if err != nil || len(got) != 1 {
				t.Fatalf("claims: %v, %v", got, err)
			}
		})
	}
}

func TestDestructiveCallsStopWhenForeignClaimsCannotBeRead(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	want := errors.New("claims unavailable")
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{}, want }
	if err := c.composeDown(context.Background(), "project", true); !errors.Is(err, want) {
		t.Fatalf("down: %v", err)
	}
	if err := c.removeVolume(context.Background(), "data", func(string) {}); !errors.Is(err, want) {
		t.Fatalf("volume: %v", err)
	}
	if len(downRuns(runner)) != 0 {
		t.Fatal("compose down ran")
	}
}

func TestComposeDownRefusesAProjectVolumeAnotherEnvironmentHolds(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	docker := newComposeDocker()
	docker.projectVolumes = []string{"pgdata"}
	c, _ := composeController(t, runner, docker)
	c.Foreign = func(context.Context) (Foreign, error) {
		return Foreign{Claims: map[state.Claim]bool{{Kind: state.ClaimVolume, Name: "pgdata"}: true}}, nil
	}
	err := c.composeDown(context.Background(), "shared", true)
	if err == nil || !strings.Contains(err.Error(), "name pgdata is taken") {
		t.Fatalf("down: %v", err)
	}
	if len(downRuns(runner)) != 0 {
		t.Fatal("compose down ran")
	}
}

func TestIngressAdmissionChecksTheConnectorName(t *testing.T) {
	m := withIngress(t, removeApps())
	claim := state.Claim{Kind: state.ClaimService, Name: m.ServiceName("test", manifest.ConnectorAppName, "connector")}
	c := Controller{Environment: "test", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }}
	req := Request{Kind: "ingress", RequestID: "ingress", Manifest: m}
	if _, _, err := c.Admit(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "name "+claim.Name+" is taken") {
		t.Fatalf("admit: %v", err)
	}
}

type schemaRevertImageSpy struct {
	*revertFake
	inspections int
}

func (s *schemaRevertImageSpy) InspectImage(ctx context.Context, name string) (engine.Image, error) {
	s.inspections++
	return s.revertFake.positiveEngine.InspectImage(ctx, name)
}

func TestSchemaRevertChecksRollbackNamesBeforeImageWork(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, _, err := s.Admit(ctx, state.Admission{RequestID: "old", ActorID: "admin", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, old.ID, state.Outcome{State: "succeeded", SchemaAfter: "v1", Release: releaseOf(oldImage)}); err != nil {
		t.Fatal(err)
	}
	rel, err := s.Applied(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	migration, _, err := s.Admit(ctx, state.Admission{RequestID: "migration", ActorID: "admin", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	source, _ := json.Marshal(struct {
		Manifest manifest.Manifest `json:"manifest"`
		Image    string            `json:"image"`
	}{revertManifest(), newImage})
	if err := s.RecordIntent(ctx, migration.ID, state.Intent{Ordinal: 9000, Kind: "schema_source", Desired: source}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, migration.ID, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage)}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE releases SET created=1000 WHERE id=?; UPDATE operations SET started=2000 WHERE id=?`, rel.ID, migration.ID); err != nil {
		t.Fatal(err)
	}
	spy := &schemaRevertImageSpy{revertFake: newRevertFake(s, "", nil)}
	c := Controller{Store: s, Engine: spy, Environment: "test"}
	claim := state.Claim{Kind: state.ClaimService, Name: "proj-test-app-web"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	c.Reserve = func(context.Context, []state.Claim) ([]state.Claim, error) {
		return nil, Refusal("name proj-test-app-web is taken by another project or environment on this server")
	}
	req := Request{Kind: "rollback", App: "app", RevertSchema: true, TargetRelease: rel.ID}
	if _, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "name proj-test-app-web is taken") {
		t.Fatalf("plan: %v", err)
	}
	out := c.runSchemaRevert(ctx, state.Operation{}, Actor{ID: "admin", Admin: true}, req)
	if out.Err != "name proj-test-app-web is taken by another project or environment on this server" || spy.inspections != 0 {
		t.Fatalf("outcome: %+v; image inspections: %d", out, spy.inspections)
	}
	if _, err := db.Exec(`UPDATE applied SET release_id=? WHERE environment=? AND app=?`, rel.ID, "test", "app"); err != nil {
		t.Fatal(err)
	}
	networkClaim := state.Claim{Kind: state.ClaimNetwork, Name: "overlay"}
	c.Foreign = func(context.Context) (Foreign, error) {
		return Foreign{Claims: map[state.Claim]bool{networkClaim: true}}, nil
	}
	c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
		if !slices.Contains(claims, networkClaim) {
			return nil, errors.New("missing migration network")
		}
		return nil, Refusal("name overlay is taken by another project or environment on this server")
	}
	if _, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req); err == nil || !strings.Contains(err.Error(), "name overlay is taken") {
		t.Fatalf("schema-only plan: %v", err)
	}
	out = c.runSchemaRevert(ctx, state.Operation{}, Actor{ID: "admin", Admin: true}, req)
	if out.Err != "name overlay is taken by another project or environment on this server" || spy.inspections != 0 {
		t.Fatalf("schema-only outcome: %+v; image inspections: %d", out, spy.inspections)
	}
}

type removedComposeNetworkEngine struct{ *composeDocker }

func (e *removedComposeNetworkEngine) InspectNetwork(_ context.Context, name string) (engine.Network, error) {
	if name == "compose-net" || name == "external-net" {
		return engine.Network{}, &engine.StatusError{Code: 404}
	}
	return engine.Network{ID: "net"}, nil
}
func TestComposeDownReleasesOnlyNetworksConfirmedGone(t *testing.T) {
	ctx := context.Background()
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"networks":{"gone":{"name":"compose-net"},"kept":{"name":"external-net","external":true}}}`, hashes: hashOne}
	docker := newComposeDocker()
	c, s := composeController(t, runner, docker)
	c.Engine = &removedComposeNetworkEngine{composeDocker: docker}
	claims := []state.Claim{{Kind: state.ClaimComposeProject, Name: "project"}, {Kind: state.ClaimNetwork, Name: "compose-net"}, {Kind: state.ClaimNetwork, Name: "external-net"}}
	if err := s.AddClaims(ctx, claims); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.composeDir("project"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.composeDown(ctx, "project", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claims(ctx)
	if err != nil || !slices.Equal(got, []state.Claim{{Kind: state.ClaimNetwork, Name: "external-net"}}) {
		t.Fatalf("claims: %v, %v", got, err)
	}
}

func TestRecordedComposeRemovalRefusesForeignProjectAndVolume(t *testing.T) {
	for _, kind := range []string{state.ClaimComposeProject, state.ClaimVolume} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			runner := &fakeCompose{config: configOne, hashes: hashOne}
			docker := newComposeDocker()
			docker.projectVolumes = []string{"shop-test-db_data"}
			c, s := composeController(t, runner, docker)
			m := composeApp(false)
			raw, _ := json.Marshal(m)
			deploy, _, err := s.Admit(ctx, state.Admission{RequestID: "deploy", ActorID: "admin", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Finish(ctx, deploy.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Digest: composeDigest(composeFileOne), Manifest: raw}}); err != nil {
				t.Fatal(err)
			}
			claim := state.Claim{Kind: kind, Name: "shop-test-db"}
			if kind == state.ClaimVolume {
				claim.Name = "shop-test-db_data"
			}
			c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "remove", ActorID: "admin", Kind: "remove", App: "db", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			out := c.runRemove(ctx, op, Actor{ID: "admin", Admin: true}, Request{App: "db", Volumes: true})
			if err := s.Finish(ctx, op.ID, out); err != nil {
				t.Fatal(err)
			}
			if out.State != "failed" || !strings.Contains(out.Err, "name "+claim.Name+" is taken") || len(downRuns(runner)) != 0 || !docker.volumes["shop-test-db_data"] {
				t.Fatalf("outcome %+v; down: %v; volumes: %v", out, downRuns(runner), docker.volumes)
			}
		})
	}
}

func TestAnInvalidExplicitNetworkIsRefusedBeforeClaims(t *testing.T) {
	network := strings.Repeat("a", 64)
	claim := state.Claim{Kind: state.ClaimNetwork, Name: network}
	c := Controller{Environment: "test", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }}
	req := Request{App: "api", RequestID: "r", Manifest: manifest.Manifest{Project: "shop", Network: network, Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {}}}}}}
	if _, err := c.Plan(context.Background(), Actor{ID: "admin"}, req); err == nil || err.Error() != "invalid project or network" {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := c.Admit(context.Background(), Actor{ID: "admin"}, req); err == nil || err.Error() != "invalid project or network" {
		t.Fatalf("admit: %v", err)
	}
}

func TestAdmissionRetryFindsRecordedRequestBeforeNameChecks(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	one := 1
	req := Request{App: "api", Version: "sha256:" + strings.Repeat("a", 64), RequestID: "retry", Manifest: manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	copyReq := req
	copyReq.RequestID = ""
	raw, err := json.Marshal(copyReq)
	if err != nil {
		t.Fatal(err)
	}
	recorded, _, err := s.Admit(ctx, state.Admission{RequestID: req.RequestID, ActorID: "actor", Kind: "deploy", RequestHash: sha256.Sum256(raw), PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{Kind: state.ClaimService, Name: "shop-test-api-web"}
	c := Controller{Store: s, Environment: "test", Foreign: func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }}
	got, created, err := c.Admit(ctx, Actor{ID: "actor"}, req)
	if err != nil || created || got.ID != recorded.ID {
		t.Fatalf("retry: %v, %v, %v", got.ID, created, err)
	}
	_, _, err = c.Admit(ctx, Actor{ID: "different"}, req)
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("actor conflict: %v", err)
	}
	req.Version = "changed"
	_, _, err = c.Admit(ctx, Actor{ID: "actor"}, req)
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("hash conflict: %v", err)
	}
}

type volumeRetryBarrier struct {
	*volumeDocker
	first  chan struct{}
	resume chan struct{}
	calls  int
}

func (d *volumeRetryBarrier) RemoveVolume(ctx context.Context, name string) error {
	d.calls++
	if d.calls == 1 {
		close(d.first)
		<-d.resume
		return &engine.StatusError{Code: 409}
	}
	return d.volumeDocker.RemoveVolume(ctx, name)
}
func TestVolumeRemovalReservesAcrossRetries(t *testing.T) {
	ctx := context.Background()
	a, b := openStore(t), openStore(t)
	var mu sync.Mutex
	reserve := func(own, other *state.Store) func(context.Context, []state.Claim) ([]state.Claim, error) {
		return func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
			mu.Lock()
			defer mu.Unlock()
			held, err := other.Claims(ctx)
			if err != nil {
				return nil, err
			}
			for _, claim := range claims {
				if slices.Contains(held, claim) {
					return nil, Refusal("name data is taken by another project or environment on this server")
				}
			}
			return own.AddClaimsNew(ctx, claims)
		}
	}
	docker := &volumeRetryBarrier{volumeDocker: &volumeDocker{loggedEngine: &loggedEngine{calls: &callLog{}}, volumes: map[string]string{"data": ""}}, first: make(chan struct{}), resume: make(chan struct{})}
	c := Controller{Store: a, Engine: docker, Reserve: reserve(a, b), Foreign: func(ctx context.Context) (Foreign, error) {
		held, err := b.Claims(ctx)
		f := Foreign{Claims: map[state.Claim]bool{}}
		for _, cl := range held {
			f.Claims[cl] = true
		}
		return f, err
	}}
	result := make(chan error, 1)
	go func() { result <- c.removeVolume(ctx, "data", func(string) {}) }()
	<-docker.first
	_, otherErr := reserve(b, a)(ctx, []state.Claim{{Kind: state.ClaimVolume, Name: "data"}})
	close(docker.resume)
	removeErr := <-result
	if otherErr == nil {
		t.Fatalf("another environment reserved the volume during the retry; removal: %v", removeErr)
	}
	if removeErr != nil {
		t.Fatalf("removal: %v", removeErr)
	}
}

func TestComposeDownReservesBeforeDockerChanges(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
		if !slices.Contains(claims, state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}) {
			t.Fatalf("project missing: %v", claims)
		}
		return nil, Refusal("name shared is taken by another project or environment on this server")
	}
	err := c.composeDown(context.Background(), "shared", false)
	if err == nil || !strings.Contains(err.Error(), "name shared is taken") {
		t.Fatalf("compose down: %v", err)
	}
	for _, run := range runner.runs {
		if run.Args[0] == "down" {
			t.Fatal("down ran before reservation")
		}
	}
}

type composeNetworkInventory struct {
	*composeDocker
	path string
}

func (d *composeNetworkInventory) Get(_ context.Context, path string, out any) error {
	d.path = path
	return json.Unmarshal([]byte(`[{"Name":"managed-net"}]`), out)
}
func TestComposeDownInventoriesManagedNetworksWithoutFiles(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	base := newComposeDocker()
	c, _ := composeController(t, runner, base)
	docker := &composeNetworkInventory{composeDocker: base}
	c.Engine = docker
	c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
		if slices.Contains(claims, state.Claim{Kind: state.ClaimNetwork, Name: "managed-net"}) {
			return nil, Refusal("name managed-net is taken by another project or environment on this server")
		}
		return claims, nil
	}
	err := c.composeDown(context.Background(), "shared", false)
	if err == nil || !strings.Contains(err.Error(), "name managed-net is taken") {
		t.Fatalf("down: %v", err)
	}
	if !strings.HasPrefix(docker.path, "/networks?") || !strings.Contains(docker.path, "com.docker.compose.project") {
		t.Fatalf("inventory path: %s", docker.path)
	}
	if len(downRuns(runner)) != 0 {
		t.Fatal("down ran")
	}
}
func (d *composeNetworkInventory) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{}, &engine.StatusError{Code: 404}
}
func TestComposeDownKeepsExternalNetworkClaims(t *testing.T) {
	ctx := context.Background()
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"networks":{"ext":{"name":"external-net","external":true}}}`, hashes: hashOne}
	base := newComposeDocker()
	c, s := composeController(t, runner, base)
	c.Engine = &composeNetworkInventory{composeDocker: base}
	c.Reserve = s.AddClaimsNew
	if err := os.MkdirAll(c.composeDir("shared"), 0700); err != nil {
		t.Fatal(err)
	}
	managed := state.Claim{Kind: state.ClaimNetwork, Name: "managed-net"}
	external := state.Claim{Kind: state.ClaimNetwork, Name: "external-net"}
	if err := s.AddClaims(ctx, []state.Claim{managed, external}); err != nil {
		t.Fatal(err)
	}
	if err := c.composeDown(ctx, "shared", false); err != nil {
		t.Fatal(err)
	}
	held, err := s.Claims(ctx)
	if err != nil || slices.Contains(held, managed) || !slices.Contains(held, external) {
		t.Fatalf("held: %v, %v", held, err)
	}
}

func TestFirstDeployRefusesExistingServiceAndKeepsItsClaim(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "deploy", ActorID: "actor", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 10000, Kind: "oneoff", Desired: json.RawMessage(`{"HostConfig":{"Mounts":[]}}`)}); err != nil {
		t.Fatal(err)
	}
	c := Controller{rolloutHold: zeroTestRolloutHold(), Store: s, Engine: &positiveEngine{spec: json.RawMessage(`{"Name":"shop-test-api-web"}`)}, Environment: "test"}
	c.Reserve = func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
		return s.AddClaimsNew(ctx, claims)
	}
	one := 1
	req := Request{App: "api", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "shop", Network: "overlay", Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	out, _ := c.runDeploy(ctx, op, Actor{ID: "actor"}, req, false)
	if out.Err != "service name is unavailable" {
		t.Fatalf("outcome: %+v", out)
	}
	claims, err := s.Claims(ctx)
	if err != nil || !slices.Contains(claims, state.Claim{Kind: state.ClaimService, Name: "shop-test-api-web"}) {
		intents, _ := s.Intents(ctx, op.ID)
		t.Fatalf("claims: %v, %v; intents: %+v", claims, err, intents)
	}
}

func TestOneOffCommandsReserveNamedMountsBeforeCreatingContainers(t *testing.T) {
	t.Run("job", func(t *testing.T) {
		c, f := jobFixture(t)
		called := false
		c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
			called = true
			if !slices.Contains(claims, state.Claim{Kind: state.ClaimVolume, Name: "proj-test_uploads"}) {
				t.Errorf("claims: %v", claims)
			}
			return nil, Refusal("name proj-test_uploads is taken by another project or environment on this server")
		}
		out, err := c.RunJob(context.Background(), imageJobManifest(), "report", webJob, 7)
		if !called || err != nil || !strings.Contains(out.Detail, "name proj-test_uploads is taken") || f.callList() != "" {
			t.Fatalf("called=%v outcome=%+v err=%v Docker=%s", called, out, err, f.callList())
		}
	})
	t.Run("run", func(t *testing.T) {
		m := runManifest("main")
		role := m.Apps["web"].Roles["main"]
		role.Volumes = []string{"uploads:/data"}
		m.Apps["web"].Roles["main"] = role
		c, f, s := runFixture(t, m)
		called := false
		c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
			called = true
			return nil, Refusal("name proj-test_uploads is taken by another project or environment on this server")
		}
		op, _, err := c.Admit(context.Background(), runAdmin, runRequest("oneoff", "true"))
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		result, err := s.Operation(context.Background(), op.ID)
		if err != nil || !called || result.State != "failed" || f.callList() != "" {
			t.Fatalf("called=%v result=%+v err=%v Docker=%s", called, result, err, f.callList())
		}
	})
}

type unknownClaimEngine struct{ *positiveEngine }

func (*unknownClaimEngine) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return engine.ServiceSnapshot{}, errors.New("inspection unavailable")
}
func TestRecoveryKeepsClaimsWhenAbsenceCannotBeConfirmed(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	_, _, err := s.Admit(ctx, state.Admission{RequestID: "recover-unknown", ActorID: "actor", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{Kind: state.ClaimService, Name: "unknown"}
	if err = s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: &unknownClaimEngine{positiveEngine: &positiveEngine{}}}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	held, err := s.Claims(ctx)
	if err != nil || !slices.Contains(held, claim) {
		t.Fatalf("claims: %v, %v", held, err)
	}
}

func TestRemovalSettlesClaimOfAlreadyAbsentDeclaredVolume(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimVolume, Name: "shop-test_data"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "remove", ActorID: "actor", Kind: "remove", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	docker := &volumeDocker{loggedEngine: &loggedEngine{calls: &callLog{}}, volumes: map[string]string{}}
	c := Controller{Store: s, Engine: docker, Environment: "test", Reserve: s.AddClaimsNew}
	app := manifest.App{Kind: "image", Roles: map[string]manifest.Role{"web": {Volumes: []string{"data:/data"}}}}
	r := &removal{m: manifest.Manifest{Project: "shop"}, app: app, plan: RemovePlan{Kind: "image"}}
	journal := 0
	out := c.removeApp(ctx, op.ID, Request{App: "api", Volumes: true}, r, func(string) {}, &journal)
	if out.State != "succeeded" {
		t.Fatalf("outcome: %+v", out)
	}
	held, err := s.Claims(ctx)
	if err != nil || slices.Contains(held, claim) {
		t.Fatalf("claims: %v, %v", held, err)
	}
}

type missingNetworkDestroyEngine struct{ *failedFirstDeployEngine }

func (*missingNetworkDestroyEngine) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{}, &engine.StatusError{Code: 404}
}

func TestDestroySweepsAbsentClaimsButKeepsEnvironmentNetwork(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claims := []state.Claim{{Kind: state.ClaimNetwork, Name: "shop-test"}, {Kind: state.ClaimService, Name: "shop-test-api-web"}, {Kind: state.ClaimVolume, Name: "shop-test_data"}}
	if err := s.AddClaims(ctx, claims); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "destroy", ActorID: "admin", Kind: "destroy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: &missingNetworkDestroyEngine{&failedFirstDeployEngine{positiveEngine: &positiveEngine{}}}, Project: "shop", Environment: "test"}
	out := c.runDestroy(ctx, op, Actor{ID: "admin", Admin: true}, Request{Confirm: c.id().String()})
	if out.State != "succeeded" {
		t.Fatalf("destroy: %+v", out)
	}
	held, err := s.Claims(ctx)
	if err != nil || !slices.Equal(held, claims[:1]) {
		t.Fatalf("claims: %v %v", held, err)
	}
}

func TestApplyDoesNotCreateAnOperation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	c := Controller{Store: s, Engine: &positiveEngine{}, Environment: "test"}
	req := ApplyRequest{Manifest: dockerManifest(nil), DryRun: true}
	dry, err := c.ApplySettings(ctx, Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.AuditLog(ctx, 10)
	if err != nil || len(audit) != 0 {
		t.Fatalf("dry-run audit: %v %v", audit, err)
	}
	req.DryRun = false
	applied, err := c.ApplySettings(ctx, Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dry.Changes, applied.Changes) {
		t.Fatalf("changes: %v vs %v", dry.Changes, applied.Changes)
	}
	audit, err = s.AuditLog(ctx, 10)
	if err != nil || len(audit) != 1 {
		t.Fatalf("apply audit: %v %v", audit, err)
	}
}

func TestFailedRestoreReleasesOnlyItsAddedRemovedVolume(t *testing.T) {
	for _, preheld := range []bool{false, true} {
		t.Run(strconv.FormatBool(preheld), func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			claim := state.Claim{Kind: state.ClaimVolume, Name: "restored-1"}
			if preheld {
				if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
					t.Fatal(err)
				}
			}
			docker := newComposeDocker()
			docker.volumes["restored-1"] = false
			c := Controller{Store: s, Engine: docker, Environment: "test", Restorer: &fakeRestorer{err: errors.Join(errors.New("failed"), backup.ErrRestoreVolumeRemoved)}, Reserve: s.AddClaimsNew}
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "restore", ActorID: "admin", Kind: "restore", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			out := c.runRestore(ctx, op, restoreAdmin, restoreRequest("restore"))
			if out.State != "failed" {
				t.Fatalf("outcome: %+v", out)
			}
			held, err := s.Claims(ctx)
			if err != nil || slices.Contains(held, claim) != preheld {
				t.Fatalf("claims: %v, %v", held, err)
			}
		})
	}
}

func TestRecoveryDoesNotReserveOrReleaseClaims(t *testing.T) {
	for _, kind := range []string{"remove", "restore"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "recover", ActorID: "admin", Kind: kind, PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			claim := state.Claim{Kind: state.ClaimService, Name: "gone"}
			i := state.Intent{Ordinal: 1, Kind: removeServiceKind, Name: "gone", ServiceID: "id", Desired: json.RawMessage(`{}`)}
			docker := &restoreDocker{positiveEngine: &positiveEngine{}, volumes: map[string]map[string]string{}}
			if kind == "restore" {
				claim = state.Claim{Kind: state.ClaimVolume, Name: "new"}
				docker.volumes["new"] = map[string]string{"berthy.restore-operation": op.ID}
				i = state.Intent{Ordinal: 1, Kind: restoreKind, Name: "new", Desired: json.RawMessage(`{"into":"new"}`)}
			}
			if err = s.AddClaims(ctx, []state.Claim{claim}); err != nil {
				t.Fatal(err)
			}
			if err = s.RecordIntent(ctx, op.ID, i); err != nil {
				t.Fatal(err)
			}
			calls := 0
			c := Controller{Store: s, Engine: docker, Reserve: func(context.Context, []state.Claim) ([]state.Claim, error) { calls++; return nil, nil }, FinishReservation: func(context.Context, []state.Claim, []state.Claim, []state.Claim) error { calls++; return nil }}
			if err = c.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			held, err := s.Claims(ctx)
			if err != nil || calls != 0 || !slices.Contains(held, claim) {
				t.Fatalf("calls=%d claims=%v err=%v", calls, held, err)
			}
		})
	}
}

func TestComposeDownKeepsExternalVolumeClaimWhenAbsent(t *testing.T) {
	ctx := context.Background()
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"volumes":{"ext":{"name":"external-data","external":true}}}`, hashes: hashOne}
	docker := newComposeDocker()
	c, s := composeController(t, runner, docker)
	c.Reserve = s.AddClaimsNew
	claim := state.Claim{Kind: state.ClaimVolume, Name: "external-data"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.composeDir("shared"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.composeDown(ctx, "shared", true); err != nil {
		t.Fatal(err)
	}
	held, err := s.Claims(ctx)
	if err != nil || !slices.Contains(held, claim) {
		t.Fatalf("claims: %v, %v", held, err)
	}
}

func TestRepeatedComposeRemoveReleasesPreviouslyLeasedManagedNetwork(t *testing.T) {
	ctx := context.Background()
	c, s, runner, docker := composeRemoveFixture(t, false)
	runner.config = `{"services":{"database":{"image":"alpine"}},"networks":{"managed":{"name":"compose-net"},"external":{"name":"external-net","external":true}}}`
	c.Engine = &removedComposeNetworkEngine{docker}
	c.Reserve = s.AddClaimsNew
	managed := state.Claim{Kind: state.ClaimNetwork, Name: "compose-net"}
	external := state.Claim{Kind: state.ClaimNetwork, Name: "external-net"}
	if err := s.AddClaims(ctx, []state.Claim{managed, external}); err != nil {
		t.Fatal(err)
	}
	active := true
	c.FinishReservation = func(ctx context.Context, leased, candidates, eligible []state.Claim) error {
		var release []state.Claim
		for _, claim := range eligible {
			if slices.Contains(candidates, claim) && !(active && claim == managed) {
				release = append(release, claim)
			}
		}
		return s.ReleaseClaims(ctx, release)
	}
	remove := func(id string) {
		t.Helper()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "admin", Kind: "remove", App: "db", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		out := c.runRemove(ctx, op, Actor{ID: "admin", Admin: true}, Request{App: "db"})
		if out.State != "succeeded" {
			t.Fatalf("remove: %+v", out)
		}
		if err = s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
	}
	remove("first")
	held, err := s.Claims(ctx)
	if err != nil || !slices.Contains(held, managed) || !slices.Contains(held, external) {
		t.Fatalf("first claims: %v %v", held, err)
	}
	active = false
	remove("second")
	held, err = s.Claims(ctx)
	if err != nil || slices.Contains(held, managed) || !slices.Contains(held, external) {
		t.Fatalf("second claims: %v %v", held, err)
	}
}

func TestFailedComposeRemovalKeepsClaims(t *testing.T) {
	ctx := context.Background()
	c, s, runner, docker := composeRemoveFixture(t, false)
	runner.config = `{"services":{"database":{"image":"alpine"}},"networks":{"managed":{"name":"compose-net"}}}`
	runner.downErr = errors.New("down failed")
	c.Engine = &removedComposeNetworkEngine{docker}
	c.Reserve = s.AddClaimsNew
	claim := state.Claim{Kind: state.ClaimNetwork, Name: "compose-net"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "failed-remove", ActorID: "admin", Kind: "remove", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if out := c.runRemove(ctx, op, Actor{ID: "admin", Admin: true}, Request{App: "db"}); out.State != "failed" {
		t.Fatalf("outcome: %+v", out)
	}
	held, err := s.Claims(ctx)
	if err != nil || !slices.Contains(held, claim) {
		t.Fatalf("claims: %v, %v", held, err)
	}
}

func TestRetryReleasesNetworkInventoriedBeforeInterruptedDown(t *testing.T) {
	ctx := context.Background()
	c, s, runner, docker := composeRemoveFixture(t, false)
	runner.config = `{"services":{"database":{"image":"alpine"}}}`
	c.Engine = &removedComposeNetworkEngine{docker}
	c.Reserve = s.AddClaimsNew
	claim := state.Claim{Kind: state.ClaimNetwork, Name: "compose-net"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	old, _, err := s.Admit(ctx, state.Admission{RequestID: "old-remove", ActorID: "admin", Kind: "remove", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, old.ID, state.Intent{Ordinal: 1, Kind: composeDownKind, Name: "shop-test-db", Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(map[string]any{"targets": []state.Claim{claim}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, old.ID, 1, "issued", receipt); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, old.ID, state.Outcome{State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "retry-remove", ActorID: "admin", Kind: "remove", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if out := c.runRemove(ctx, op, Actor{ID: "admin", Admin: true}, Request{App: "db"}); out.State != "succeeded" {
		t.Fatalf("outcome: %+v", out)
	}
	held, err := s.Claims(ctx)
	if err != nil || slices.Contains(held, claim) {
		t.Fatalf("claims: %v, %v", held, err)
	}
}

func TestAgentRefusesSharedComposeProjects(t *testing.T) {
	c := Controller{Environment: "prod"}
	m := manifest.Manifest{Project: "demo", Apps: map[string]manifest.App{"alpha": {Kind: "compose", File: "alpha.yml"}, "beta": {Kind: "compose", File: "beta.yml", ProjectName: "demo-prod-alpha"}}}
	req := Request{Kind: "deploy", App: "alpha", RequestID: "duplicate", Manifest: m}
	want := "Compose project demo-prod-alpha is shared by apps alpha and beta"
	if _, err := c.Plan(context.Background(), Actor{ID: "a"}, req); err == nil || err.Error() != want || !PublicMessage(err.Error()) {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := c.Admit(context.Background(), Actor{ID: "a"}, req); err == nil || err.Error() != want {
		t.Fatalf("admit: %v", err)
	}
	if PublicMessage(want + " extra") {
		t.Fatal("accepted unsafe message")
	}
}

func TestComposeDeployRefusesProjectOfOtherAppliedApp(t *testing.T) {
	ctx := context.Background()
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	actor := Actor{ID: "admin", Admin: true}
	app := manifest.App{Kind: "compose", File: "compose.yaml", ProjectName: "shared", Deploy: manifest.Deploy{DeployTimeout: "30s", DrainTimeout: "5s"}}
	alpha := manifest.Manifest{Project: "shop", Network: "berthy", Apps: map[string]manifest.App{"alpha": app}}
	first := Request{RequestID: "alpha", Kind: "deploy", App: "alpha", Manifest: alpha, ComposeFile: composeFileOne}
	op, _, err := admit(t, c, ctx, actor, first)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	done, err := s.Operation(ctx, op.ID)
	if err != nil || done.State != "succeeded" {
		t.Fatalf("alpha: %+v %v", done, err)
	}
	beta := manifest.Manifest{Project: "shop", Network: "berthy", Apps: map[string]manifest.App{"beta": app}}
	req := Request{RequestID: "beta", Kind: "deploy", App: "beta", Manifest: beta, ComposeFile: composeFileOne, DryRun: true}
	want := "Compose project shared is shared by apps alpha and beta"
	if _, err = c.Plan(ctx, actor, req); err == nil || err.Error() != want {
		t.Errorf("dry run: %v", err)
	}
	req.DryRun = false
	if _, _, err = admit(t, c, ctx, actor, req); err == nil || err.Error() != want {
		t.Errorf("admission: %v", err)
	}
	if err = c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.upRuns() != 1 {
		t.Errorf("compose up ran %d times", runner.upRuns())
	}
	first.RequestID = "alpha-again"
	first.DryRun = true
	if _, err := c.Plan(ctx, actor, first); err != nil {
		t.Errorf("redeploy of the same app: %v", err)
	}
}

func TestComposeRollbackRefusesProjectOfOtherAppliedApp(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ID: "admin", Admin: true}
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	for _, tc := range []struct{ id, app, project, file string }{{"beta-old", "beta", "shared", composeFileOne}, {"beta-new", "beta", "beta-new", composeFileTwo}, {"alpha", "alpha", "shared", composeFileOne}} {
		a := manifest.App{Kind: "compose", File: "compose.yaml", ProjectName: tc.project, Deploy: manifest.Deploy{DeployTimeout: "30s", DrainTimeout: "5s"}}
		req := Request{RequestID: tc.id, Kind: "deploy", App: tc.app, Manifest: manifest.Manifest{Project: "shop", Network: "berthy", Apps: map[string]manifest.App{tc.app: a}}, ComposeFile: tc.file}
		op, _, err := admit(t, c, ctx, actor, req)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := s.Operation(ctx, op.ID)
		if err != nil || done.State != "succeeded" {
			t.Fatalf("%s: %+v %v", tc.id, done, err)
		}
	}
	want := "Compose project shared is shared by apps alpha and beta"
	req := Request{RequestID: "back", Kind: "rollback", App: "beta", DryRun: true}
	if _, err := c.Plan(ctx, actor, req); err == nil || err.Error() != want {
		t.Errorf("dry run: %v", err)
	}
	req.DryRun = false
	if _, _, err := admit(t, c, ctx, actor, req); err == nil || err.Error() != want {
		t.Errorf("admission: %v", err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.upRuns() != 3 {
		t.Errorf("compose up ran %d times", runner.upRuns())
	}
}

func TestRetriedRemoveKeepsComposeClaimOfAppliedApp(t *testing.T) {
	ctx := context.Background()
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	docker := newComposeDocker(engine.ComposeContainer{ID: "alpha", Service: "database", State: "running"})
	runner.onDown = func(engine.ComposeRun) { docker.containers = nil }
	c, s := composeController(t, runner, docker)
	c.Reserve = s.AddClaimsNew
	actor := Actor{ID: "admin", Admin: true}
	deploy := func(name string) {
		t.Helper()
		app := manifest.App{Kind: "compose", File: "compose.yaml", ProjectName: "shared", Deploy: manifest.Deploy{DeployTimeout: "30s", DrainTimeout: "5s"}}
		req := Request{RequestID: "deploy-" + name, Kind: "deploy", App: name, Manifest: manifest.Manifest{Project: "shop", Network: "berthy", Apps: map[string]manifest.App{name: app}}, ComposeFile: composeFileOne}
		op, _, err := admit(t, c, ctx, actor, req)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := s.Operation(ctx, op.ID)
		if err != nil || done.State != "succeeded" {
			t.Fatalf("deploy %s: %+v %v", name, done, err)
		}
	}
	remove := func(id string) {
		t.Helper()
		req := Request{RequestID: id, Kind: "remove", App: "alpha"}
		op, _, err := admit(t, c, ctx, actor, req)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := s.Operation(ctx, op.ID)
		if err != nil || done.State != "succeeded" {
			t.Fatalf("remove: %+v %v", done, err)
		}
	}
	claim := state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}
	deploy("alpha")
	remove("remove-alpha")
	docker.containers = []engine.ComposeContainer{{ID: "beta", Service: "database", State: "running"}}
	deploy("beta")
	claims, err := s.Claims(ctx)
	if err != nil || !slices.Contains(claims, claim) {
		t.Fatalf("after beta: %v %v", claims, err)
	}
	docker.containers = nil
	remove("retry-alpha")
	claims, err = s.Claims(ctx)
	if err != nil || !slices.Contains(claims, claim) {
		t.Fatalf("retry released beta's claim: %v %v", claims, err)
	}
}

func TestDestroySweepKeepsComposeClaimOfAppliedApp(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "beta", ActorID: "admin", Kind: "deploy", App: "beta", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	rel := &state.Release{App: "beta", Digest: "digest", Manifest: []byte(`{"project":"shop","apps":{"beta":{"kind":"compose","project_name":"shared"}}}`)}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: rel}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: newComposeDocker(), Environment: "test"}
	if err = c.sweepClaims(ctx); err != nil {
		t.Fatal(err)
	}
	held, err := s.Claims(ctx)
	if err != nil || !slices.Contains(held, claim) {
		t.Fatalf("sweep released applied project: %v %v", held, err)
	}
}

func TestComposeClaimIsKeptWhenAppliedReleaseCannotBeRead(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	claim := state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "unreadable", ActorID: "admin", Kind: "deploy", App: "beta", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "beta", Digest: "digest", Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: newComposeDocker(), Environment: "test"}
	c.settle(ctx, []state.Claim{claim})
	held, err := s.Claims(ctx)
	if err != nil || !slices.Contains(held, claim) {
		t.Fatalf("claim after unreadable release: %v %v", held, err)
	}
}

func TestComposeNetworkModeReservesExistingNetwork(t *testing.T) {
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine","network_mode":"peer-overlay"}}}`, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	claim := state.Claim{Kind: state.ClaimNetwork, Name: "peer-overlay"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	req := composeReq("mode", composeFileOne, composeApp(false))
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil {
		t.Fatal("peer network accepted")
	}
	c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
		if !slices.Contains(claims, claim) {
			t.Fatal("network mode was not reserved")
		}
		return claims, nil
	}
	c.runCompose(context.Background(), state.Operation{}, Actor{ID: "admin", Admin: true}, req, false)
}

func TestComposeDeployReservesEnvironmentNetwork(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	claim := state.Claim{Kind: state.ClaimNetwork, Name: "peer-overlay"}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	m := composeApp(false)
	m.Network = claim.Name
	req := composeReq("env-network", composeFileOne, m)
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil {
		t.Fatal("peer environment network accepted")
	}
	c.Reserve = func(_ context.Context, claims []state.Claim) ([]state.Claim, error) {
		if !slices.Contains(claims, claim) {
			t.Fatal("environment network was not reserved")
		}
		return claims, nil
	}
	c.runCompose(context.Background(), state.Operation{}, Actor{ID: "admin", Admin: true}, req, false)
}

type composeNetworkIDLookup struct{ *composeDocker }

func (e composeNetworkIDLookup) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{ID: "peerid", Name: "peer-overlay", Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}

func TestComposeRejectsEnvironmentNetworkID(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	c.Engine = composeNetworkIDLookup{newComposeDocker()}
	m := composeApp(false)
	m.Network = "peerid"
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, composeReq("id", composeFileOne, m))
	if err == nil {
		t.Fatal("accepted network ID")
	}
}

func TestValidInterpolatedComposeCollisionNeverDisclosesSecret(t *testing.T) {
	ctx := context.Background()
	const secret = "private-valid-network"
	runner := &fakeCompose{hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	c.Compose = &interpolatedNetworkRunner{runner}
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "DB_URL"}, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env = manifest.Env{Secret: []manifest.SecretVar{{Var: "DATABASE_URL", Secret: "DB_URL"}}}
	m.Apps["db"] = app
	req := composeReq("private-conflict", composeFileOne, m)
	claim := state.Claim{Kind: state.ClaimNetwork, Name: secret}
	c.Foreign = func(context.Context) (Foreign, error) { return Foreign{Claims: map[state.Claim]bool{claim: true}}, nil }
	var log []string
	c.Log = func(s string) { log = append(log, s) }
	want := "Compose networks.backend name is taken by another project or environment on this server"
	for _, admit := range []bool{false, true} {
		var err error
		if admit {
			_, _, err = c.Admit(ctx, Actor{ID: "admin", Admin: true}, req)
		} else {
			_, err = c.Plan(ctx, Actor{ID: "admin", Admin: true}, req)
		}
		if err == nil || err.Error() != want {
			t.Fatalf("admit %v: %v", admit, err)
		}
	}
	c.Reserve = func(context.Context, []state.Claim) ([]state.Claim, error) {
		return nil, ClaimConflict{Claim: claim}
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "private-op", ActorID: "admin", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	out := c.runCompose(ctx, op, Actor{ID: "admin", Admin: true}, req, false)
	if err := s.Finish(ctx, op.ID, out); err != nil {
		t.Fatal(err)
	}
	recorded, err := s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if out.Err != want || !PublicMessage(want) || PublicMessage(want+" extra") || strings.Contains(string(body), secret) || strings.Contains(strings.Join(log, "\n"), secret) {
		t.Fatalf("out=%+v record=%s logs=%v", out, body, log)
	}
}

type composeModeIDLookup struct{ *composeDocker }

func (e composeModeIDLookup) InspectNetwork(ctx context.Context, name string) (engine.Network, error) {
	if name == "peerid" {
		return engine.Network{ID: "peerid", Name: "peer-overlay", Driver: "overlay", Scope: "swarm", Attachable: true}, nil
	}
	return e.composeDocker.InspectNetwork(ctx, name)
}

func TestComposeNetworkModeRejectsNetworkID(t *testing.T) {
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine","network_mode":"peerid"}}}`, hashes: hashOne}
	c, _ := composeController(t, runner, newComposeDocker())
	c.Engine = composeModeIDLookup{newComposeDocker()}
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, composeReq("mode-id", composeFileOne, composeApp(false))); err == nil {
		t.Fatal("accepted network ID in network_mode")
	}
}

func TestComposeDefaultNetworkModeNeedsNoLookupOrClaim(t *testing.T) {
	cfg := composeConfigFile{Services: map[string]composeConfigService{"web": {NetworkMode: "default"}}}
	if err := (&Controller{}).checkComposeNetworkModes(context.Background(), cfg); err != nil {
		t.Fatalf("default lookup: %v", err)
	}
	claims, err := composeConfigNames("project", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(claims, state.Claim{Kind: state.ClaimNetwork, Name: "default"}) {
		t.Fatalf("default claimed: %v", claims)
	}
}

func TestComposeExternalNetworkRejectsNetworkID(t *testing.T) {
	runner := &fakeCompose{config: `{"services":{"database":{"image":"alpine"}},"networks":{"peer":{"name":"peerid","external":true}}}`, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	c.Engine = composeModeIDLookup{newComposeDocker()}
	req := composeReq("external-id", composeFileOne, composeApp(false))
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil {
		t.Fatal("plan accepted network ID")
	}
	assertComposeNetworkRefusal(t, c, s, runner, req, "Compose network unavailable or name mismatch")
}

func assertComposeNetworkRefusal(t *testing.T, c *Controller, s *state.Store, runner *fakeCompose, req Request, want string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: req.RequestID, ActorID: "admin", Kind: "deploy", App: req.App, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	out := c.runCompose(ctx, op, Actor{ID: "admin", Admin: true}, req, false)
	if err := s.Finish(ctx, op.ID, out); err != nil {
		t.Fatal(err)
	}
	if out.State != "failed" || out.Err != want || runner.upRuns() != 0 {
		t.Fatalf("execution: %+v; up=%d", out, runner.upRuns())
	}
}

func TestComposeAllowsNewNetwork(t *testing.T) {
	cfg := composeConfigFile{Networks: map[string]composeConfigVolume{"new": {Name: "new-net"}}}
	c := &Controller{Engine: &composeNetworkInventory{}}
	if err := c.checkComposeNetworkModes(context.Background(), cfg); err != nil {
		t.Fatalf("new network: %v", err)
	}
}

type nonAttachableComposeNetwork struct{ *composeDocker }

func (e nonAttachableComposeNetwork) InspectNetwork(ctx context.Context, name string) (engine.Network, error) {
	n, err := e.composeDocker.InspectNetwork(ctx, name)
	n.Attachable = false
	return n, err
}

func TestComposeRejectsNonAttachableEnvironmentNetwork(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	c.Engine = nonAttachableComposeNetwork{newComposeDocker()}
	req := composeReq("non-attachable", composeFileOne, composeApp(false))
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil {
		t.Fatal("non-attachable network accepted")
	}
	assertComposeNetworkRefusal(t, c, s, runner, req, "overlay network unavailable")
}

type claimReservationError struct{ claim state.Claim }

func (e claimReservationError) Error() string {
	return "name " + e.claim.Name + " is taken by another project or environment on this server"
}
func (e claimReservationError) ConflictingClaim() state.Claim { return e.claim }

func TestComposeConflictIdentifiesTheClaimKindAndServiceMode(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		claim        state.Claim
		label        string
	}{
		{"mode", `{"services":{"database":{"image":"alpine","network_mode":"shared"}}}`, state.Claim{Kind: state.ClaimNetwork, Name: "shared"}, "services.database.network_mode"},
		{"volume", `{"services":{"database":{"image":"alpine"}},"volumes":{"data":{"name":"shared"}},"networks":{"backend":{"name":"shared"}}}`, state.Claim{Kind: state.ClaimVolume, Name: "shared"}, "volumes.data"},
		{"network", `{"services":{"database":{"image":"alpine"}},"volumes":{"data":{"name":"shared"}},"networks":{"backend":{"name":"shared"}}}`, state.Claim{Kind: state.ClaimNetwork, Name: "shared"}, "networks.backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeCompose{config: tc.config, hashes: hashOne}
			c, _ := composeController(t, runner, newComposeDocker())
			c.Foreign = func(context.Context) (Foreign, error) {
				return Foreign{Claims: map[state.Claim]bool{tc.claim: true}}, nil
			}
			c.Reserve = func(context.Context, []state.Claim) ([]state.Claim, error) {
				return nil, claimReservationError{tc.claim}
			}
			want := "Compose " + tc.label + " name is taken by another project or environment on this server"
			req := composeReq("collision", composeFileOne, composeApp(false))
			if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err == nil || err.Error() != want {
				t.Fatalf("plan: %v", err)
			}
			out := c.runCompose(context.Background(), state.Operation{}, Actor{ID: "admin", Admin: true}, req, false)
			if out.Err != want || !PublicMessage(want) {
				t.Fatalf("execution: %+v", out)
			}
		})
	}
}
