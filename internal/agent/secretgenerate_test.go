package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/state"
)

// errReader fails every read, so a test sees that the generator reports a failed random source.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("no random source") }

// TestGenerateSecretFormatsAreExact checks each format against values worked out by hand, from a reader that gives fixed bytes.
func TestGenerateSecretFormatsAreExact(t *testing.T) {
	seq := make([]byte, 32)
	for i := range seq {
		seq[i] = byte(i)
	}
	for _, tc := range []struct {
		name   string
		in     []byte
		n      int
		format string
		want   string
	}{
		{"hex of 32 bytes", seq, 32, "hex", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"},
		{"base64 is URL-safe", []byte{0xfb, 0xff, 0xbf}, 3, "base64", "-_-_"},
		{"base64 has no padding", []byte{0xff, 0xff}, 2, "base64", "__8"},
		// 255 is 248 or more, so it is refused and skipped: 0 is A, 247 and 61 are 9 (index 61 of the alphabet), 62 wraps to A.
		{"alnum skips bytes from 248 up", []byte{255, 0, 247, 61, 62, 1}, 4, "alnum", "A99A"},
	} {
		got, err := generateSecret(bytes.NewReader(tc.in), tc.n, tc.format)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestGenerateSecretAlnumIsUnbiased checks that the 248 bytes an alnum character can come from give every one of the 62
// characters exactly four times, so no character is more likely than another.
func TestGenerateSecretAlnumIsUnbiased(t *testing.T) {
	in := make([]byte, 248)
	for i := range in {
		in[i] = byte(i)
	}
	got, err := generateSecret(bytes.NewReader(in), 248, "alnum")
	if err != nil || len(got) != 248 {
		t.Fatalf("generate: %d characters, %v", len(got), err)
	}
	counts := map[rune]int{}
	for _, c := range got {
		counts[c]++
	}
	if len(counts) != 62 {
		t.Fatalf("%d distinct characters, want 62", len(counts))
	}
	for c, n := range counts {
		if n != 4 {
			t.Fatalf("character %q appears %d times, want 4", c, n)
		}
	}
}

// TestGenerateSecretRandomValuesHaveTheRightShape checks the generator against crypto/rand: the length and the characters.
func TestGenerateSecretRandomValuesHaveTheRightShape(t *testing.T) {
	for _, tc := range []struct {
		n       int
		format  string
		pattern string
	}{
		{32, "hex", `^[0-9a-f]{64}$`},
		{16, "base64", `^[A-Za-z0-9_-]{22}$`},
		{20, "alnum", `^[A-Za-z0-9]{20}$`},
	} {
		re := regexp.MustCompile(tc.pattern)
		first, err := generateSecret(rand.Reader, tc.n, tc.format)
		if err != nil || !re.MatchString(first) {
			t.Fatalf("%s: %q, %v; want it to match %s", tc.format, first, err, tc.pattern)
		}
		second, err := generateSecret(rand.Reader, tc.n, tc.format)
		if err != nil || second == first {
			t.Fatalf("%s: two values are equal (%q) or failed: %v", tc.format, second, err)
		}
	}
}

// TestGenerateSecretReportsAFailedRandomSource checks that a random source that fails stops the generator with its error, and
// that an unknown format is an error.
func TestGenerateSecretReportsAFailedRandomSource(t *testing.T) {
	if _, err := generateSecret(errReader{}, 32, "hex"); err == nil {
		t.Fatal("a failed random source gave a value")
	}
	if _, err := generateSecret(rand.Reader, 32, "uuid"); err == nil {
		t.Fatal("an unknown format gave a value")
	}
}

// TestSecretGenerateStoresARandomValueOfTheRequestedLengthAndFormat checks the default and the other formats: the answer never
// holds the value, and the stored value has the length and the characters of its format.
func TestSecretGenerateStoresARandomValueOfTheRequestedLengthAndFormat(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	for _, tc := range []struct {
		name, body, pattern string
	}{
		{"GEN_HEX", `{"name":"GEN_HEX"}`, `^[0-9a-f]{64}$`},
		{"GEN_B64", `{"name":"GEN_B64","bytes":16,"format":"base64"}`, `^[A-Za-z0-9_-]{22}$`},
		{"GEN_ALNUM", `{"name":"GEN_ALNUM","bytes":20,"format":"alnum"}`, `^[A-Za-z0-9]{20}$`},
		{"GEN_MAX", `{"name":"GEN_MAX","bytes":256,"format":"hex"}`, `^[0-9a-f]{512}$`},
	} {
		w := f.call("POST", "/secrets/generate", apAdminToken, tc.body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		v, ok := storedValue(t, f, tc.name)
		if !ok || !regexp.MustCompile(tc.pattern).MatchString(v) {
			t.Fatalf("%s: stored %q, %v; want it to match %s", tc.name, v, ok, tc.pattern)
		}
		if strings.Contains(w.Body.String(), v) {
			t.Fatalf("%s: the answer holds the value: %s", tc.name, w.Body.String())
		}
	}
	w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"GEN_DEFAULT"}`)
	if !sameJSON(t, w.Body.Bytes(), []byte(`{"name":"GEN_DEFAULT","bytes":32,"format":"hex","replaced":false}`)) {
		t.Fatalf("default answer: %s", w.Body.String())
	}
}

// TestSecretGenerateRefusesAStoredSecretUnlessItReplacesIt checks that a stored secret is refused with 409 without replace, and
// that with replace it is given a new value.
func TestSecretGenerateRefusesAStoredSecretUnlessItReplacesIt(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "GEN_PASS"}, []byte("in-use-value")); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"GEN_PASS"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "secret GEN_PASS exists; --replace replaces it") {
		t.Fatalf("refused generate: %d %s", w.Code, w.Body.String())
	}
	if v, _ := storedValue(t, f, "GEN_PASS"); v != "in-use-value" {
		t.Fatalf("a refused generate changed the value: %q", v)
	}
	w = f.call("POST", "/secrets/generate", apAdminToken, `{"name":"GEN_PASS","replace":true}`)
	if w.Code != 200 || !sameJSON(t, w.Body.Bytes(), []byte(`{"name":"GEN_PASS","bytes":32,"format":"hex","replaced":true}`)) {
		t.Fatalf("replacing generate: %d %s", w.Code, w.Body.String())
	}
	if v, _ := storedValue(t, f, "GEN_PASS"); v == "in-use-value" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v) {
		t.Fatalf("replaced value: %q", v)
	}
}

// TestSecretGenerateRefusesARegistryPassword checks that a name stored as a registry password is refused in the dry run and in
// the generate, with or without replace, and that its value does not change.
func TestSecretGenerateRefusesARegistryPassword(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "GHCR_TOKEN", Registry: "ghcr.io"}, []byte("pull")); err != nil {
		t.Fatal(err)
	}
	const message = "GHCR_TOKEN is a registry password and cannot be generated"
	for _, body := range []string{
		`{"name":"GHCR_TOKEN","replace":true}`,
		`{"name":"GHCR_TOKEN","replace":true,"dry_run":true}`,
		`{"name":"GHCR_TOKEN","dry_run":true}`,
	} {
		if w := f.call("POST", "/secrets/generate", apAdminToken, body); w.Code != 400 || !strings.Contains(w.Body.String(), message) {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if v, _ := storedValue(t, f, "GHCR_TOKEN"); v != "pull" {
		t.Fatalf("the registry password changed: %q", v)
	}
}

// TestSecretGenerateRefusesABadRequestAndStoresNothing checks each rule the agent enforces on a generate request.
func TestSecretGenerateRefusesABadRequestAndStoresNothing(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	for _, tc := range []struct {
		name, body string
	}{
		{"no name", `{}`},
		{"a name that is not a secret name", `{"name":"1BAD"}`},
		{"the tunnel token", `{"name":"TUNNEL_TOKEN"}`},
		{"bytes below 16", `{"name":"A","bytes":15}`},
		{"bytes above 256", `{"name":"A","bytes":257}`},
		{"bytes of zero", `{"name":"A","bytes":0}`},
		{"bytes not a number", `{"name":"A","bytes":"32"}`},
		{"an unknown format", `{"name":"A","format":"uuid"}`},
		{"a format in capitals", `{"name":"A","format":"HEX"}`},
		{"a registry", `{"name":"A","registry":"ghcr.io"}`},
		{"a value", `{"name":"A","value":"typed"}`},
		{"a dry run of a bad bytes", `{"name":"A","bytes":1,"dry_run":true}`},
	} {
		if w := f.call("POST", "/secrets/generate", apAdminToken, tc.body); w.Code != 400 {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if got, err := f.s.Secrets(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("stored %v after refused generates (%v)", got, err)
	}
}

// TestSecretGenerateDryRunPlansWithoutStoring checks the dry run: the plan names the secret, whether it exists, and the length and
// format, and holds no value. Nothing is stored and a stored value does not change.
func TestSecretGenerateDryRunPlansWithoutStoring(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "OLD"}, []byte("old-value")); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"NEW_PASS","bytes":24,"format":"base64","dry_run":true}`)
	if w.Code != 200 || !sameJSON(t, w.Body.Bytes(), []byte(`{"action":"secret generate","name":"NEW_PASS","exists":false,"bytes":24,"format":"base64"}`)) {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	w = f.call("POST", "/secrets/generate", apAdminToken, `{"name":"OLD","dry_run":true}`)
	if w.Code != 200 || !sameJSON(t, w.Body.Bytes(), []byte(`{"action":"secret generate","name":"OLD","exists":true,"bytes":32,"format":"hex"}`)) {
		t.Fatalf("dry run of a stored secret: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "NEW_PASS"); ok {
		t.Fatal("a dry run stored a secret")
	}
	if v, _ := storedValue(t, f, "OLD"); v != "old-value" {
		t.Fatalf("a dry run changed OLD: %q", v)
	}
}

// TestSecretGenerateNeedsAnAdminToken checks that only an admin token generates, as it sets a secret.
func TestSecretGenerateNeedsAnAdminToken(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	for _, token := range []string{apReadToken, apDeployToken} {
		if w := f.call("POST", "/secrets/generate", token, `{"name":"A"}`); w.Code != 403 {
			t.Fatalf("token %s: %d %s", token[:12], w.Code, w.Body.String())
		}
	}
	if _, ok := storedValue(t, f, "A"); ok {
		t.Fatal("a refused generate stored a secret")
	}
}

// TestSecretGenerateIsAuditedByItsOwnKind checks that a generate leaves one succeeded row of its kind, by the admin who asked.
func TestSecretGenerateIsAuditedByItsOwnKind(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"GEN_PASS"}`); w.Code != 200 {
		t.Fatalf("generate: %d %s", w.Code, w.Body.String())
	}
	seen := 0
	for _, row := range f.audit() {
		if row == "alice secret_generate:GEN_PASS succeeded" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("audit %v, want one secret_generate:GEN_PASS row by alice", f.audit())
	}
}

// TestSecretGenerateOnAProtectedEnvironmentIsHeldUntilApproved checks the approval flow: an agent admin's generate is held, its
// plan and the approval list name the secret and no value, nothing is stored until approval, and approving stores a value of the
// requested shape. The approval's answer holds no value either.
func TestSecretGenerateOnAProtectedEnvironmentIsHeldUntilApproved(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/secrets/generate", apAgentAdmin, `{"name":"GEN_PASS","bytes":20,"format":"alnum"}`)
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret generate","name":"GEN_PASS","exists":false,"bytes":20,"format":"alnum"}`)) {
		t.Fatalf("plan: %s", h.Plan)
	}
	if _, ok := storedValue(t, f, "GEN_PASS"); ok {
		t.Fatal("the held generate stored a secret")
	}
	list := f.call("GET", "/approvals", apReadToken, "")
	if !strings.Contains(list.Body.String(), `"summary":"secret generate GEN_PASS"`) || !strings.Contains(list.Body.String(), `"kind":"secret generate"`) {
		t.Fatalf("list: %s", list.Body.String())
	}
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	v, ok := storedValue(t, f, "GEN_PASS")
	if !ok || !regexp.MustCompile(`^[A-Za-z0-9]{20}$`).MatchString(v) {
		t.Fatalf("approved GEN_PASS: %q, %v", v, ok)
	}
	if strings.Contains(list.Body.String()+w.Body.String(), v) {
		t.Fatal("the approval shows the value")
	}
}

// TestHeldSecretGenerateForAStoredSecretFailsOnApproval checks that a held generate without replace is refused when a person
// approves it, the approval is recorded as failed, and the stored value does not change.
func TestHeldSecretGenerateForAStoredSecretFailsOnApproval(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "GEN_PASS"}, []byte("in-use")); err != nil {
		t.Fatal(err)
	}
	h := f.hold("POST", "/secrets/generate", apAgentAdmin, `{"name":"GEN_PASS"}`)
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret generate","name":"GEN_PASS","exists":true,"bytes":32,"format":"hex"}`)) {
		t.Fatalf("plan: %s", h.Plan)
	}
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "secret GEN_PASS exists; --replace replaces it") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if status := f.status(h.ID); status != "failed" {
		t.Fatalf("approval status %q, want failed", status)
	}
	if v, _ := storedValue(t, f, "GEN_PASS"); v != "in-use" {
		t.Fatalf("the stored value changed: %q", v)
	}
}

// TestDeletingASecretNamedGenerateIsStillADelete checks that the route for a secret named generate is told apart by its method:
// a delete of that secret is still a delete, in the approvals and in the store.
func TestDeletingASecretNamedGenerateIsStillADelete(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "generate"}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	h := f.hold("DELETE", "/secrets/generate", apAgentAdmin, `{}`)
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret delete","name":"generate","exists":true}`)) {
		t.Fatalf("plan: %s", h.Plan)
	}
	list := f.call("GET", "/approvals", apReadToken, "")
	if !strings.Contains(list.Body.String(), `"kind":"secret delete"`) || !strings.Contains(list.Body.String(), `"summary":"secret delete generate"`) {
		t.Fatalf("list: %s", list.Body.String())
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "generate"); ok {
		t.Fatal("the secret named generate was not deleted")
	}
}

func TestGenerateDeclaredRejectsInvalidManifest(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, `{}`)
	if w.Code != 400 {
		t.Fatalf("invalid manifest: %d %s, want 400", w.Code, w.Body.String())
	}
}

func TestSecretGenerateByNameIsRefusedWhenAReaderWasDeployed(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "deploy-db", ActorID: "a", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`
	if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: []byte(m)}}); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"DB_PASS"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "app db was deployed here and may hold the value of DB_PASS") {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("refusal stored secret")
	}
}

func TestGenerateDeclaredKeepsAStoredSecret(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "DB_PASS"}, []byte("old-value")); err != nil {
		t.Fatal(err)
	}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}}}`
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"keep"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if v, _ := storedValue(t, f, "DB_PASS"); v != "old-value" {
		t.Fatal("stored value changed")
	}
}

func TestGenerateDeclaredRefusesWhenAReaderWasDeployed(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "db-deploy", ActorID: "a", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`
	if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: []byte(m)}}); err != nil {
		t.Fatal(err)
	}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}}}`
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"refuse"`) || !strings.Contains(w.Body.String(), `"app":"db"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("refused secret stored")
	}
}

func TestSecretGenerateByNameDryRunNamesTheFinding(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "db-deploy", ActorID: "a", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`
	if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: []byte(m)}}); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"DB_PASS","dry_run":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"app":"db"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("dry run stored secret")
	}
}

type generateEngine struct{ planEngine }

func (generateEngine) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
}

func TestGenerateDeclaredAfterAFailureHalfway(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = generateEngine{}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["A_PASS","B_PASS"]}}},"secrets":{"A_PASS":{"generate":{"format":"hex"}},"B_PASS":{"generate":{"format":"hex"}}}}}`
	previous := secretRandom
	t.Cleanup(func() { secretRandom = previous })
	secretRandom = bytes.NewReader(make([]byte, 32))
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 503 {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	first, ok := storedValue(t, f, "A_PASS")
	if !ok {
		t.Fatal("first missing")
	}
	if _, ok := storedValue(t, f, "B_PASS"); ok {
		t.Fatal("second stored despite failed random")
	}
	secretRandom = previous
	w = f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"keep"`) || !strings.Contains(w.Body.String(), `"outcome":"generate"`) {
		t.Fatalf("second request: %d %s", w.Code, w.Body.String())
	}
	again, _ := storedValue(t, f, "A_PASS")
	if again != first {
		t.Fatal("first value changed")
	}
	if _, ok := storedValue(t, f, "B_PASS"); !ok {
		t.Fatal("second missing")
	}
	for _, name := range []string{"A_PASS", "B_PASS"} {
		count := 0
		for _, row := range f.audit() {
			if strings.Contains(row, "secret_generate:"+name+" ") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s audit count %d", name, count)
		}
	}
	if strings.Contains(w.Body.String(), first) {
		t.Fatal("response exposes value")
	}
}

func TestHeldGenerateDeclaredPlanIsTheDryRun(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	f.ctrl.Engine = generateEngine{}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["A_PASS"]}}},"secrets":{"A_PASS":{"generate":{"format":"hex"}}}}}`
	dry := strings.TrimSuffix(body, "}") + `,"dry_run":true}`
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, dry)
	if w.Code != 200 {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	h := f.hold("POST", "/secrets/generate-declared", apAgentAdmin, body)
	if !sameJSON(t, h.Plan, w.Body.Bytes()) {
		t.Fatalf("held %s, dry %s", h.Plan, w.Body.String())
	}
	if _, ok := storedValue(t, f, "A_PASS"); ok {
		t.Fatal("planning stored a secret")
	}
	w = f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != 200 {
		t.Fatalf("approval: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "A_PASS"); !ok {
		t.Fatal("approval did not generate")
	}
}

func TestGenerateDeclaredSeesAReaderOfAnOlderRelease(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = generateEngine{}
	manifests := []string{
		`{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`,
		`{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}}}}}`,
	}
	for i, m := range manifests {
		op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: fmt.Sprint(i), ActorID: "a", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: []byte(m)}}); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}}}`
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"refuse"`) || !strings.Contains(w.Body.String(), `"app":"db"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("secret stored")
	}
}

func TestGenerateDeclaredRefusesAfterAFailedFirstDeploy(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "failed", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["A_PASS"]}}},"secrets":{"A_PASS":{"generate":{"format":"hex"}}}}}`
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"refuse"`) || !strings.Contains(w.Body.String(), `"app":"api"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "A_PASS"); ok {
		t.Fatal("secret stored")
	}
}

func TestSecretGenerateByNameWithReplaceIsNotGuarded(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "old", ActorID: "a", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`
	if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: []byte(m)}}); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"DB_PASS","replace":true}`)
	if w.Code != 200 {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); !ok {
		t.Fatal("secret missing")
	}
}

func TestGenerateDeclaredNeedsAnAdminToken(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	for _, token := range []string{apReadToken, apDeployToken} {
		w := f.call("POST", "/secrets/generate-declared", token, `{}`)
		if w.Code != 403 {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
	}
}
