package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
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
	if value, ok := storedValue(t, f, "A_PASS"); !ok || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value) {
		t.Fatal("approval did not generate a value matching its plan")
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

type generateComposeEngine struct {
	generateEngine
	volumes   map[string]bool
	forbidden bool
}

func (e *generateComposeEngine) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	if e.forbidden {
		panic("foreign container lookup")
	}
	return nil, nil
}
func (e *generateComposeEngine) ProjectVolumes(context.Context, string) ([]string, error) {
	if e.forbidden {
		panic("foreign project volume lookup")
	}
	return nil, nil
}
func (e *generateComposeEngine) VolumeExists(_ context.Context, name string) (bool, error) {
	if e.forbidden {
		panic("foreign volume lookup")
	}
	return e.volumes[name], nil
}

func TestSecretGenerateByNameWithAManifestAgreesWithTheDeclaredRoute(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = &generateComposeEngine{volumes: map[string]bool{"shared": true}}
	m := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"compose","file":"compose.yml","env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}}`
	files := `{"db":"volumes: {data: {external: true, name: shared}}"}`
	named := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"DB_PASS","manifest":`+m+`,"compose_files":`+files+`}`)
	if named.Code != 409 || !strings.Contains(named.Body.String(), "the volume shared of app db exists") {
		t.Fatalf("named: %d %s", named.Code, named.Body.String())
	}
	declared := f.call("POST", "/secrets/generate-declared", apAdminToken, `{"manifest":`+m+`,"compose_files":`+files+`}`)
	if declared.Code != 200 || !strings.Contains(declared.Body.String(), `"outcome":"refuse"`) || !strings.Contains(declared.Body.String(), `"volume":"shared"`) {
		t.Fatalf("declared: %d %s", declared.Code, declared.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("refusal stored secret")
	}
	named = f.call("POST", "/secrets/generate", apAdminToken, `{"name":"DB_PASS"}`)
	if named.Code != 200 {
		t.Fatalf("state-only: %d %s", named.Code, named.Body.String())
	}
}

func TestGenerateDeclaredRefusesForeignComposeProject(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = &generateComposeEngine{forbidden: true}
	f.ctrl.Foreign = func(context.Context) (deploy.Foreign, error) {
		return deploy.Foreign{Claims: map[state.Claim]bool{{Kind: state.ClaimComposeProject, Name: "foreign"}: true}}, nil
	}
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"db":{"kind":"compose","file":"compose.yml","project_name":"foreign","env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}},"compose_files":{"db":"volumes: {data: {name: private-volume}}"}}`
	w := f.call("POST", "/secrets/generate-declared", apAdminToken, body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Compose project name is taken by another project or environment on this server") || strings.Contains(w.Body.String(), "private-volume") {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("foreign refusal stored secret")
	}
}

func TestGenerateDeclaredMixedReadersAndRepeat(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = generateEngine{}
	recorded := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`
	recordAppRelease(t, f.s, "db-record", "db", recorded)
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}},"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["API_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}},"API_PASS":{"generate":{"format":"hex"}}}}}`
	previous := secretRandom
	t.Cleanup(func() { secretRandom = previous })
	secretRandom = bytes.NewReader(bytes.Repeat([]byte{0xab}, 32))
	read := func(raw string, want api.DeclaredOutcome) string {
		t.Helper()
		w := f.call("POST", "/secrets/generate-declared", apAdminToken, raw)
		var answer api.SecretsDeclared
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Action != "secret generate declared" || len(answer.Secrets) != 2 {
			t.Fatalf("unexpected response status or shape: %d", w.Code)
		}
		expected := []api.DeclaredSecretOutcome{
			{Name: "API_PASS", Outcome: want, Bytes: 32, Format: "hex"},
			{Name: "DB_PASS", Outcome: api.DeclaredRefuse, Bytes: 32, Format: "hex", App: "db"},
		}
		if !reflect.DeepEqual(answer.Secrets, expected) {
			t.Fatalf("unexpected outcomes: %+v", answer.Secrets)
		}
		return w.Body.String()
	}
	dry := read(strings.TrimSuffix(body, "}")+`,"dry_run":true}`, api.DeclaredGenerate)
	for _, name := range []string{"API_PASS", "DB_PASS"} {
		if _, ok := storedValue(t, f, name); ok {
			t.Fatalf("dry run stored %s", name)
		}
	}
	first := read(body, api.DeclaredGenerate)
	value, ok := storedValue(t, f, "API_PASS")
	if !ok || value == "" {
		t.Fatal("fresh value missing")
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("deployed reader secret stored")
	}
	again := read(body, api.DeclaredKeep)
	value2, ok := storedValue(t, f, "API_PASS")
	if !ok || value2 != value {
		t.Fatal("repeat changed stored value")
	}
	if _, ok := storedValue(t, f, "DB_PASS"); ok {
		t.Fatal("repeat stored the deployed reader's secret")
	}
	rows, err := f.s.AuditLog(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := json.Marshal(rows)
	count := 0
	for _, row := range rows {
		if row.Action == "secret_generate:API_PASS" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("generation audit count %d", count)
	}
	if strings.Contains(dry, value) || strings.Contains(first, value) || strings.Contains(again, value) || bytes.Contains(audit, []byte(value)) {
		t.Fatal("value leaked into a plan, answer or audit")
	}
}

type failingSecretInventory struct {
	*generateComposeEngine
	fail string
}

const inventoryDiagnostic = "private-docker-diagnostic"

func (e *failingSecretInventory) ComposeContainers(ctx context.Context, project string) ([]engine.ComposeContainer, error) {
	if e.fail == "containers" {
		return nil, errors.New(inventoryDiagnostic)
	}
	return e.generateComposeEngine.ComposeContainers(ctx, project)
}
func (e *failingSecretInventory) ProjectVolumes(ctx context.Context, project string) ([]string, error) {
	if e.fail == "projects" {
		return nil, errors.New(inventoryDiagnostic)
	}
	if e.fail == "project warning" {
		return nil, errors.New("volume inventory unavailable")
	}
	return e.generateComposeEngine.ProjectVolumes(ctx, project)
}
func (e *failingSecretInventory) VolumeExists(ctx context.Context, volume string) (bool, error) {
	if e.fail == "volumes" {
		return false, errors.New(inventoryDiagnostic)
	}
	return e.generateComposeEngine.VolumeExists(ctx, volume)
}
func (e *failingSecretInventory) InspectService(ctx context.Context, service string) (engine.ServiceSnapshot, error) {
	if e.fail == "service" {
		return engine.ServiceSnapshot{}, &engine.StatusError{Code: 500}
	}
	return e.generateEngine.InspectService(ctx, service)
}

func TestGenerateRoutesRefuseUnestablishedReaderState(t *testing.T) {
	for _, tc := range []struct {
		name, file, fail                            string
		omit, image, release, inconsistent, foreign bool
	}{
		{name: "omitted file", omit: true},
		{name: "malformed yaml", file: "volumes: ["},
		{name: "include", file: "include: [other.yml]"},
		{name: "braced interpolation", file: "volumes: {data: {name: '${STACK}_data'}}"},
		{name: "bare interpolation", file: "volumes: {data: {name: '$STACK'}}"},
		{name: "container lookup", file: "services: {}", fail: "containers"},
		{name: "project volume lookup", file: "services: {}", fail: "projects"},
		{name: "project volume driver warning", file: "services: {}", fail: "project warning"},
		{name: "fixed volume lookup", file: "volumes: {data: {name: shared}}", fail: "volumes"},
		{name: "service lookup", image: true, fail: "service"},
		{name: "unreadable release", file: "services: {}", release: true},
		{name: "release missing its app", file: "services: {}", inconsistent: true},
		{name: "foreign lookup", file: "services: {}", foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []string{"generate", "generate-declared"} {
				t.Run(route, func(t *testing.T) {
					f := newApprovalFixture(t, false, nil)
					f.ctrl.Engine = &failingSecretInventory{generateComposeEngine: &generateComposeEngine{}, fail: tc.fail}
					if tc.foreign {
						f.ctrl.Foreign = func(context.Context) (deploy.Foreign, error) {
							return deploy.Foreign{}, errors.New(inventoryDiagnostic)
						}
					}
					if tc.release || tc.inconsistent {
						op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "bad-release", ActorID: "a", Kind: "deploy", App: "old", PublicRequest: []byte(`{}`)})
						if err != nil {
							t.Fatal(err)
						}
						stored := `{}`
						if tc.inconsistent {
							stored = apManifestSource
						}
						if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "old", Manifest: []byte(stored)}}); err != nil {
							t.Fatal(err)
						}
					}
					app := map[string]any{"kind": "compose", "file": "compose.yml", "env": map[string]any{"secret": []string{"DB_PASS"}}}
					if tc.image {
						app = map[string]any{"kind": "image", "image": "ghcr.io/acme/db", "roles": map[string]any{"web": map[string]any{}}, "env": map[string]any{"secret": []string{"DB_PASS"}}}
					}
					m := map[string]any{"project": "demo", "network": "overlay", "apps": map[string]any{"db": app}, "secrets": map[string]any{"DB_PASS": map[string]any{"generate": map[string]any{"format": "hex"}}}}
					files := map[string]string{}
					if !tc.omit && !tc.image {
						files["db"] = tc.file
					}
					body := map[string]any{"manifest": m, "compose_files": files}
					if route == "generate" {
						body["name"] = "DB_PASS"
					}
					encoded, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					w := f.call("POST", "/secrets/"+route, apAdminToken, string(encoded))
					message := "the state of app db cannot be established, so nothing is generated"
					if tc.release || tc.inconsistent {
						message = "the recorded releases cannot be read, so nothing is generated"
					}
					if w.Code != 400 || !strings.Contains(w.Body.String(), message) || strings.Contains(w.Body.String(), inventoryDiagnostic) {
						t.Fatalf("%s: status %d, expected refusal %t, diagnostic exposed %t", route, w.Code, strings.Contains(w.Body.String(), message), strings.Contains(w.Body.String(), inventoryDiagnostic))
					}
					if _, ok := storedValue(t, f, "DB_PASS"); ok {
						t.Fatalf("%s stored secret", route)
					}
				})
			}
		})
	}
}

func TestGenerateApprovalsRejectRepeatedTopLevelFields(t *testing.T) {
	for _, tc := range []struct{ name, route, body string }{
		{"same key", "generate", `{"name":"GEN_PASS","name":"GEN_PASS"}`},
		{"case variant", "generate", `{"name":"GEN_PASS","NAME":"GEN_PASS"}`},
		{"dry run", "generate", `{"name":"GEN_PASS","dry_run":true,"DRY_RUN":true}`},
		{"declared", "generate-declared", `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}},"dry_run":false,"DRY_RUN":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, true, nil)
			w := f.call("POST", "/secrets/"+tc.route, apAgentAdmin, tc.body)
			if w.Code != 400 || !sameJSON(t, w.Body.Bytes(), []byte(`{"code":"bad_request","message":"invalid request"}`)) {
				t.Fatalf("status %d, response %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Berthy-Approval") != "" {
				t.Fatal("ambiguous request was held")
			}
			list := f.call("GET", "/approvals", apAdminToken, "")
			if strings.TrimSpace(list.Body.String()) != "[]" {
				t.Fatal("ambiguous request created an approval")
			}
		})
	}
}

func TestGenerateApprovalReplanningRejectsRepeatedFields(t *testing.T) {
	manifest := `{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`
	for _, tc := range []struct{ name, route, valid, repeated string }{
		{"named", "generate", `{"name":"GEN_PASS"}`, `{"name":"GEN_PASS","NAME":"GEN_PASS"}`},
		{"declared", "generate-declared", `{"manifest":` + manifest + `}`, `{"manifest":` + manifest + `,"dry_run":false,"DRY_RUN":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, true, nil)
			held := f.hold("POST", "/secrets/"+tc.route, apAgentAdmin, tc.valid)
			prior, err := f.s.Approval(t.Context(), held.ID)
			if err != nil {
				t.Fatal(err)
			}
			a, err := f.s.CreateApproval(t.Context(), prior, []byte(tc.repeated))
			if err != nil {
				t.Fatal(err)
			}
			w := f.call("POST", "/approvals/"+a.ID+"/approve", apAdminToken, "")
			if w.Code != 409 || !strings.Contains(w.Body.String(), "the plan can no longer be computed") || f.status(a.ID) != "failed" {
				t.Fatalf("status %d, response %s, approval %s", w.Code, w.Body.String(), f.status(a.ID))
			}
			if secrets, err := f.s.Secrets(t.Context()); err != nil || len(secrets) != 0 {
				t.Fatalf("ambiguous approval stored secrets: %v", err)
			}
		})
	}
}

func TestNamedGenerateUsesStoredAndRecordedStateBeforeMissingComposeFile(t *testing.T) {
	for _, kind := range []string{"stored", "recorded"} {
		t.Run(kind, func(t *testing.T) {
			f := newApprovalFixture(t, false, nil)
			if kind == "stored" {
				if err := f.s.PutSecret(t.Context(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "DB_PASS"}, []byte("existing")); err != nil {
					t.Fatal(err)
				}
			} else {
				recorded := `{"project":"demo","network":"overlay","apps":{"db":{"kind":"image","image":"ghcr.io/acme/db","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}}}`
				recordAppRelease(t, f.s, "db-release", "db", recorded)
			}
			body := `{"name":"DB_PASS","manifest":{"project":"demo","network":"overlay","apps":{"db":{"kind":"compose","file":"missing.yml","env":{"secret":["DB_PASS"]}}}}}`
			w := f.call("POST", "/secrets/generate", apAdminToken, body)
			want := "secret DB_PASS exists; --replace replaces it"
			if kind == "recorded" {
				want = "app db was deployed here"
			}
			if w.Code != 409 || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("status %d, expected prior-state refusal %t", w.Code, strings.Contains(w.Body.String(), want))
			}
			_, ok := storedValue(t, f, "DB_PASS")
			if ok != (kind == "stored") {
				t.Fatal("unexpected stored state")
			}
		})
	}
}

func TestHeldGenerateRefusesSingleCaseVariantDryRun(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	w := f.call("POST", "/secrets/generate", apAgentAdmin, `{"name":"GEN_PASS","DRY_RUN":false}`)
	if w.Code != 400 || strings.TrimSpace(w.Body.String()) != `{"code":"bad_request","message":"invalid request"}` {
		t.Fatalf("case variant: %d %s", w.Code, w.Body.String())
	}
	h := f.hold("POST", "/secrets/generate", apAgentAdmin, `{"name":"GEN_PASS","dry_run":false}`)
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret generate","name":"GEN_PASS","exists":false,"bytes":32,"format":"hex"}`)) {
		t.Fatalf("unexpected plan %s", h.Plan)
	}
	w = f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != 200 {
		t.Fatalf("approval status %d", w.Code)
	}
	value, ok := storedValue(t, f, "GEN_PASS")
	if !ok || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value) {
		t.Fatal("stored value does not match approved plan")
	}
}

func TestProtectedGenerationRejectsInvalidDryRunTypes(t *testing.T) {
	manifest := `{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`
	for _, route := range []string{"generate", "generate-declared"} {
		for _, tc := range []struct{ name, key, value string }{{"string", "dry_run", `"false"`}, {"number", "DRY_RUN", "1"}, {"object", "dry_run", "{}"}} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				f := newApprovalFixture(t, true, nil)
				body := `{"name":"GEN_PASS","` + tc.key + `":` + tc.value + `}`
				if route == "generate-declared" {
					body = `{"manifest":` + manifest + `,"` + tc.key + `":` + tc.value + `}`
				}
				w := f.call("POST", "/secrets/"+route, apAgentAdmin, body)
				if w.Code != 400 || !sameJSON(t, w.Body.Bytes(), []byte(`{"code":"bad_request","message":"invalid request"}`)) || w.Header().Get("Berthy-Approval") != "" {
					t.Fatalf("status %d response %s", w.Code, w.Body.String())
				}
				list := f.call("GET", "/approvals", apAdminToken, "")
				if strings.TrimSpace(list.Body.String()) != "[]" {
					t.Fatal("invalid request was held")
				}
			})
		}
	}
}

func TestProtectedGenerationAcceptsNullDryRun(t *testing.T) {
	for _, tc := range []struct{ name, route, body string }{
		{"named", "generate", `{"name":"GEN_PASS","dry_run":null}`},
		{"declared", "generate-declared", `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}},"dry_run":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, true, nil)
			w := f.call("POST", "/secrets/"+tc.route, apAgentAdmin, tc.body)
			if w.Code != 202 || w.Header().Get("Berthy-Approval") != "pending" {
				t.Fatalf("status %d response %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestGenerationRefusesHistoricalIngressReader(t *testing.T) {
	for _, route := range []string{"generate", "generate-declared"} {
		t.Run(route, func(t *testing.T) {
			f := newApprovalFixture(t, false, nil)
			f.ctrl.Engine = generateEngine{}
			recordAppRelease(t, f.s, "ingress-release", "ingress", `{"project":"demo","network":"overlay","apps":{"ingress":{"kind":"image","image":"ghcr.io/acme/edge","roles":{"web":{}},"env":{"secret":["OLD_PASS"]}}}}`)
			body := `{"name":"OLD_PASS"}`
			if route == "generate-declared" {
				body = `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["OLD_PASS"]}}},"secrets":{"OLD_PASS":{"generate":{"format":"hex"}}}}}`
			}
			w := f.call("POST", "/secrets/"+route, apAdminToken, body)
			if route == "generate" {
				if w.Code != 409 || !strings.Contains(w.Body.String(), "app ingress was deployed here") {
					t.Fatalf("named response: %d %s", w.Code, w.Body.String())
				}
			} else {
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"refuse"`) || !strings.Contains(w.Body.String(), `"app":"ingress"`) {
					t.Fatalf("declared response: %d %s", w.Code, w.Body.String())
				}
			}
			if _, ok := storedValue(t, f, "OLD_PASS"); ok {
				t.Fatal("historical reader secret was generated")
			}
		})
	}
}

func TestConnectorHistoryAllowsUnrelatedGeneration(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = generateEngine{}
	connector := `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"docker.io/cloudflare/cloudflared","roles":{"connector":{"replicas":1}},"env":{"secret":["TUNNEL_TOKEN"]}}}}`
	op, _, err := f.s.Admit(t.Context(), state.Admission{RequestID: "connector", ActorID: "admin", Kind: "deploy", App: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finish(t.Context(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "ingress", Manifest: []byte(connector)}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ route, body, name string }{
		{"generate", `{"name":"OTHER_PASS"}`, "OTHER_PASS"},
		{"generate-declared", `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["NEW_PASS"]}}},"secrets":{"NEW_PASS":{"generate":{"format":"hex"}}}}}`, "NEW_PASS"},
	} {
		w := f.call("POST", "/secrets/"+tc.route, apAdminToken, tc.body)
		if w.Code != 200 || tc.route == "generate-declared" && !strings.Contains(w.Body.String(), `"outcome":"generate"`) {
			t.Fatalf("%s status %d", tc.route, w.Code)
		}
		if _, ok := storedValue(t, f, tc.name); !ok {
			t.Fatalf("%s not generated", tc.name)
		}
	}
}

type generationServiceEngine struct {
	generateEngine
	calls    []string
	existing string
}

func (e *generationServiceEngine) InspectService(_ context.Context, name string) (engine.ServiceSnapshot, error) {
	e.calls = append(e.calls, name)
	if name == e.existing {
		return engine.ServiceSnapshot{ID: "service"}, nil
	}
	return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
}
func TestGenerationSeesGeneratedServiceWithDifferentAdoptionName(t *testing.T) {
	for _, route := range []string{"generate", "generate-declared"} {
		t.Run(route, func(t *testing.T) {
			f := newApprovalFixture(t, false, nil)
			docker := &generationServiceEngine{existing: "demo-lab-api-web"}
			f.ctrl.Engine = docker
			m := `{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"service":"adopted-service"}},"env":{"secret":["API_PASS"]}}},"secrets":{"API_PASS":{"generate":{"format":"hex"}}}}`
			body := `{"name":"API_PASS","manifest":` + m + `}`
			if route == "generate-declared" {
				body = `{"manifest":` + m + `}`
			}
			w := f.call("POST", "/secrets/"+route, apAdminToken, body)
			if route == "generate" {
				if w.Code != 409 || !strings.Contains(w.Body.String(), "app api was deployed here") {
					t.Fatalf("named status %d", w.Code)
				}
			} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"refuse"`) || !strings.Contains(w.Body.String(), `"app":"api"`) {
				t.Fatalf("declared status %d", w.Code)
			}
			if _, ok := storedValue(t, f, "API_PASS"); ok {
				t.Fatal("secret was generated")
			}
			if !reflect.DeepEqual(docker.calls, []string{"demo-lab-api-web"}) {
				t.Fatalf("inspected services %v", docker.calls)
			}
		})
	}
}
