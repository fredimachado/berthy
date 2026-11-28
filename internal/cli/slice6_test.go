package cli

import (
	"berthy/internal/scope"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/charmbracelet/x/ansi"
)

type slice6FailWriter struct{}

func (slice6FailWriter) Write([]byte) (int, error) { return 0, errors.New("fixture write failure") }

func TestCLITableColumnsAndPlainBytes(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"lab":{"url":"https://example.invalid","token_env":"SLICE6_MISSING"},"prod":{"url":"https://example.invalid","token_env":""}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TOKEN_STORE", "none")
	t.Setenv("SLICE6_MISSING", "")
	cases := []struct {
		name, raw string
		run       func(io.Writer, io.Writer) int
	}{
		{"checks", "NAME\tSTATE\tSINCE\tDETAIL\n界\thealthy\t2026-01-01T00:00:00Z\tok\né\tfailed\t2026-01-01T00:00:00Z\t\n", func(o, e io.Writer) int {
			return printChecks(o, e, []byte(`[{"name":"界","state":"healthy","detail":"ok","since":"2026-01-01T00:00:00Z"},{"name":"é","state":"failed","detail":"","since":"2026-01-01T00:00:00Z"}]`), false)
		}},
		{"jobs", "NAME\tTARGET\tSCHEDULE\tNEXT\tLAST\tWHEN\n界\tapp\ton demand\t-\tfailed: detail\t2026-01-01T00:00:00Z\né\tapp\ton demand\t-\t-\t-\n", func(o, e io.Writer) int {
			return printJobs(o, e, []byte(`[{"name":"界","app":"app","last":{"status":"failed","detail":"detail","started":"2026-01-01T00:00:00Z"}},{"name":"é","app":"app"}]`), false)
		}},
		{"approvals", "ID\tAGE\tEXPIRES IN\tACTOR\tSUMMARY\n界\t0s\t-\tactor\t\né\t0s\t-\tactor\tsummary\n", func(o, e io.Writer) int {
			return printApprovals(o, e, []byte(`[{"id":"界","actor":"actor","status":"approved","created":"9999-01-01T00:00:00Z"},{"id":"é","actor":"actor","status":"approved","created":"9999-01-01T00:00:00Z","summary":"summary"}]`), false, false)
		}},
		{"approvals all", "ID\tAGE\tEXPIRES IN\tACTOR\tSTATUS\tDECIDED BY\tSUMMARY\n界\t0s\t-\tactor\tapproved\t-\t\né\t0s\t-\tactor\tapproved\t-\tsummary\n", func(o, e io.Writer) int {
			return printApprovals(o, e, []byte(`[{"id":"界","actor":"actor","status":"approved","created":"9999-01-01T00:00:00Z"},{"id":"é","actor":"actor","status":"approved","created":"9999-01-01T00:00:00Z","summary":"summary"}]`), true, false)
		}},
		{"tokens", "NAME\tSCOPE\tAGENT\tORIGIN\tCREATED\tEXPIRES\tLAST USED\n界\tadmin\tno\t\t-\t2000-01-02 (expired)\tnever\né\tadmin\tno\t\t-\tnever\tnever\n", func(o, e io.Writer) int {
			return printTokens(o, e, []byte(`[{"name":"界","scope":"admin","expires":"2000-01-02T00:00:00Z"},{"name":"é","scope":"admin"}]`))
		}},
		{"backups", "BACKUP\tAPP\tVOLUME\tSIZE\tFINISHED\tSHA256\tKEY\n界\tapp\tcommand\t0 B\t2026-01-01T00:00:00Z\tdigest\t\né\tapp\tcommand\t0 B\t2026-01-01T00:00:00Z\t\t\nthird\tapp\tcommand\t0 B\t2026-01-01T00:00:00Z\t\tlast\n", func(o, e io.Writer) int {
			return printBackups(o, e, []byte(`[{"backup":"界","app":"app","finished":"2026-01-01T00:00:00Z","sha256":"digest"},{"backup":"é","app":"app","finished":"2026-01-01T00:00:00Z"},{"backup":"third","app":"app","finished":"2026-01-01T00:00:00Z","key":"last"}]`), false)
		}},
		{"context", "demo\tlab\thttps://example.invalid\tSLICE6_MISSING\tno\tunknown\ndemo\tprod\thttps://example.invalid\t\tno\tunknown\n", func(o, e io.Writer) int { return runContextList([]string{"--config", cfg}, o, e) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want, out, errs bytes.Buffer
			tw := tabwriter.NewWriter(&want, 0, 0, 2, ' ', 0)
			fmt.Fprint(tw, tc.raw)
			if err := tw.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := tc.run(&out, &errs); got != 0 {
				t.Fatalf("exit %d: %s", got, errs.String())
			}
			if out.String() != want.String() {
				t.Errorf("plain got %q want %q", out.String(), want.String())
			}
			if tc.name == "checks" {
				const legacy = "NAME  STATE    SINCE                 DETAIL\n界     healthy  2026-01-01T00:00:00Z  ok\né    failed   2026-01-01T00:00:00Z  \n"
				if out.String() != legacy {
					t.Errorf("checks legacy bytes: %q", out.String())
				}
			}
			out.Reset()
			slice5Destination(t, &out)
			if got := tc.run(&out, &errs); got != 0 {
				t.Fatalf("color exit %d: %s", got, errs.String())
			}
			spans := map[string][]string{
				"checks":        {"\x1b[1mNAME\x1b[0m", "\x1b[32mhealthy\x1b[0m", "\x1b[2m2026-01-01T00:00:00Z\x1b[0m"},
				"jobs":          {"\x1b[1mLAST\x1b[0m", "\x1b[31mfailed\x1b[0m: detail", "\x1b[2m2026-01-01T00:00:00Z\x1b[0m"},
				"approvals":     {"\x1b[1mID\x1b[0m", "\x1b[2m0s\x1b[0m"},
				"approvals all": {"\x1b[1mSTATUS\x1b[0m", "\x1b[32mapproved\x1b[0m"},
				"tokens":        {"\x1b[1mEXPIRES\x1b[0m", "\x1b[2m2000-01-02 (expired)\x1b[0m"},
				"backups":       {"\x1b[1mSHA256\x1b[0m", "\x1b[2mdigest\x1b[0m", "\x1b[2m2026-01-01T00:00:00Z\x1b[0m"},
				"context":       {"\x1b[2munknown\x1b[0m"},
			}
			for _, span := range spans[tc.name] {
				if !strings.Contains(out.String(), span) {
					t.Errorf("missing span %q: %q", span, out.String())
				}
			}
			if tc.name == "context" && (strings.Contains(out.String(), "\x1b[1m") || !strings.HasPrefix(out.String(), "demo ")) {
				t.Errorf("context must be headerless: %q", out.String())
			}
			rawRows := strings.Split(strings.TrimSuffix(tc.raw, "\n"), "\n")
			widths := []int{}
			for _, raw := range rawRows {
				for i, cell := range strings.Split(raw, "\t") {
					for len(widths) <= i {
						widths = append(widths, 0)
					}
					widths[i] = max(widths[i], ansi.StringWidth(cell))
				}
			}
			var aligned strings.Builder
			for _, raw := range rawRows {
				cells := strings.Split(raw, "\t")
				for len(cells) > 0 && cells[len(cells)-1] == "" {
					cells = cells[:len(cells)-1]
				}
				for i, cell := range cells {
					aligned.WriteString(cell)
					if i < len(cells)-1 {
						aligned.WriteString(strings.Repeat(" ", widths[i]-ansi.StringWidth(cell)+2))
					}
				}
				aligned.WriteByte('\n')
			}
			if got := ansi.Strip(out.String()); got != aligned.String() {
				t.Errorf("display columns got %q want %q", got, aligned.String())
			}
			if tc.name == "checks" && !strings.HasSuffix(out.String(), "\x1b[2m2026-01-01T00:00:00Z\x1b[0m\n") {
				t.Errorf("checks trailing padding: %q", out.String())
			}
			if tc.name == "backups" {
				lines := strings.Split(out.String(), "\n")
				if len(lines) != 5 || !strings.HasSuffix(lines[1], "\x1b[2mdigest\x1b[0m") || !strings.HasSuffix(lines[2], "\x1b[2m2026-01-01T00:00:00Z\x1b[0m") || !strings.HasSuffix(lines[3], "  last") {
					t.Errorf("backup empty cells: %q", out.String())
				}
			}
			if tc.name == "jobs" && !strings.Contains(out.String(), "\x1b[31mfailed\x1b[0m: detail") {
				t.Errorf("job detail styling: %q", out.String())
			}
			fail := &slice6FailWriter{}
			if got := tc.run(fail, &errs); got != 1 {
				t.Errorf("plain failed writer exit %d", got)
			}
			slice5Destination(t, fail)
			errs.Reset()
			if got := tc.run(fail, &errs); got != 1 || !strings.Contains(errs.String(), "fixture write failure") {
				t.Errorf("colored failed writer exit %d, error %q", got, errs.String())
			}
		})
	}
	for _, tc := range []struct {
		name, want string
		run        func(io.Writer, io.Writer) int
	}{
		{"checks", "no checks yet\n", func(o, e io.Writer) int { return printChecks(o, e, []byte(`[]`), false) }},
		{"jobs", "no jobs\n", func(o, e io.Writer) int { return printJobs(o, e, []byte(`[]`), false) }},
		{"approvals", "no pending approvals\n", func(o, e io.Writer) int { return printApprovals(o, e, []byte(`[]`), false, false) }},
		{"approvals all", "no approvals\n", func(o, e io.Writer) int { return printApprovals(o, e, []byte(`[]`), true, false) }},
		{"tokens", "no tokens\n", func(o, e io.Writer) int { return printTokens(o, e, []byte(`[]`)) }},
		{"backups", "no backups\n", func(o, e io.Writer) int { return printBackups(o, e, []byte(`[]`), false) }},
	} {
		t.Run("empty "+tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			slice5Destination(t, &out)
			if code := tc.run(&out, &errs); code != 0 || out.String() != tc.want {
				t.Errorf("exit %d output %q stderr %q", code, out.String(), errs.String())
			}
		})
	}
}

func TestCLITablePreservesCellWhitespace(t *testing.T) {
	var table cliTable
	table.row("left", "", "value  ", "", "")
	table.row("", "", "")
	table.row("longer", "middle", "end")
	var out bytes.Buffer
	if err := table.write(&out, cliStyle{color: true}); err != nil {
		t.Fatal(err)
	}
	const want = "left            value  \n\nlonger  middle  end\n"
	if out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

func TestBatchAndFollowLogStyleAgree(t *testing.T) {
	record := `{"role":"web","container_id":"abcdefghijklmn","timestamp":"2026-01-01T00:00:00Z","stream":"stderr","message":"hello"}`
	stream := "event: log\ndata: " + record + "\n\nevent: done\ndata: {\"complete\":true}\n\n"
	var out, errs bytes.Buffer
	if printLogLines(&out, &errs, []byte("["+record+"]")) != 0 {
		t.Fatal(errs.String())
	}
	plain := out.String()
	if plain != "2026-01-01T00:00:00Z web.abcdefghijkl [stderr] hello\n" {
		t.Errorf("plain %q", plain)
	}
	out.Reset()
	if readStream(context.Background(), strings.NewReader(stream), &out, &errs, false) != 0 {
		t.Fatal(errs.String())
	}
	if out.String() != plain {
		t.Errorf("plain follow got %q want %q", out.String(), plain)
	}
	out.Reset()
	if readStream(context.Background(), strings.NewReader(stream), &out, &errs, true) != 0 {
		t.Fatal(errs.String())
	}
	if out.String() != record+"\n" {
		t.Errorf("json %q", out.String())
	}
	out.Reset()
	slice5Destination(t, &out)
	if printLogLines(&out, &errs, []byte("["+record+"]")) != 0 {
		t.Fatal(errs.String())
	}
	batch := out.String()
	out.Reset()
	if readStream(context.Background(), strings.NewReader(stream), &out, &errs, false) != 0 {
		t.Fatal(errs.String())
	}
	if batch != out.String() {
		t.Errorf("batch %q != follow %q", batch, out.String())
	}
	if !strings.Contains(batch, "\x1b[31m[stderr]\x1b[0m hello") {
		t.Errorf("missing log style: %q", batch)
	}
	t.Run("metadata and JSON", testBatchAndFollowLogMetadataSpans)
}

func testBatchAndFollowLogMetadataSpans(t *testing.T) {
	records := []string{
		`{"role":"web","container_id":"abcdefghijklmn","timestamp":"2026-01-01T00:00:00Z","stream":"stdout","message":"hello\u001bworld"}`,
		`{"role":"web","container_id":"abcdefghijklmn","timestamp":"2026-01-01T00:00:01Z","stream":"stderr","message":"again"}`,
		`{"role":"api","container_id":"zyxwvutsrqponm","timestamp":"2026-01-01T00:00:02Z","stream":"stdout","message":"other"}`,
	}
	body := "[" + strings.Join(records, ",") + "]"
	stream := ""
	for _, record := range records {
		stream += "event: log\ndata: " + record + "\n\n"
	}
	stream += "event: done\ndata: {\"complete\":true}\n\n"
	u, err := url.Parse("http://fixture.invalid/logs")
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	slice5Destination(t, &out)
	read := func(streaming, jsonMode bool) string {
		out.Reset()
		errs.Reset()
		payload := body
		if streaming {
			payload = stream
		}
		client := &http.Client{Transport: progressRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header)}, nil
		})}
		if code := runReadRequest(context.Background(), client, u, "fixture-token", "logs", scope.ID{}, streaming, jsonMode, &out, &errs); code != 0 {
			t.Fatalf("read exit %d: %s", code, errs.String())
		}
		return out.String()
	}
	batch := read(false, false)
	follow := read(true, false)
	if batch != follow {
		t.Errorf("batch %q != follow %q", batch, follow)
	}
	lines := strings.Split(strings.TrimSuffix(batch, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines: %q", batch)
	}
	sourcePattern := regexp.MustCompile(`\x1b\[(3[1-6]|9[1-6])m([^\x1b]+)\x1b\[0m`)
	var spans []string
	for i, line := range lines {
		timestamp := fmt.Sprintf("2026-01-01T00:00:0%dZ", i)
		if !strings.HasPrefix(line, "\x1b[2m"+timestamp+"\x1b[0m ") {
			t.Errorf("timestamp not muted: %q", line)
		}
		span := sourcePattern.FindString(line)
		if span == "" {
			t.Errorf("source span missing: %q", line)
		}
		spans = append(spans, span)
	}
	if spans[0] != spans[1] || !strings.Contains(spans[0], "web.abcdefghijkl") || !strings.Contains(spans[2], "api.zyxwvutsrqpo") {
		t.Errorf("unstable source spans: %q", spans)
	}
	if !strings.HasSuffix(lines[0], "\x1b[0m hello\\x1bworld") || !strings.HasSuffix(lines[1], "\x1b[31m[stderr]\x1b[0m again") || !strings.HasSuffix(lines[2], "\x1b[0m other") {
		t.Errorf("message reset or sanitization: %q", batch)
	}
	wantPlain := "2026-01-01T00:00:00Z web.abcdefghijkl hello\\x1bworld\n2026-01-01T00:00:01Z web.abcdefghijkl [stderr] again\n2026-01-01T00:00:02Z api.zyxwvutsrqpo other\n"
	if got := ansi.Strip(batch); got != wantPlain {
		t.Errorf("plain text %q", got)
	}
	var plain bytes.Buffer
	wantBatchJSON := `[
  {
    "role": "web",
    "container_id": "abcdefghijklmn",
    "timestamp": "2026-01-01T00:00:00Z",
    "stream": "stdout",
    "message": "hello\u001bworld"
  },
  {
    "role": "web",
    "container_id": "abcdefghijklmn",
    "timestamp": "2026-01-01T00:00:01Z",
    "stream": "stderr",
    "message": "again"
  },
  {
    "role": "api",
    "container_id": "zyxwvutsrqponm",
    "timestamp": "2026-01-01T00:00:02Z",
    "stream": "stdout",
    "message": "other"
  }
]` + "\n"
	if got := read(false, true); got != wantBatchJSON {
		t.Errorf("batch JSON %q", got)
	}
	for _, record := range records {
		plain.WriteString(record + "\n")
	}
	if got := read(true, true); got != plain.String() {
		t.Errorf("follow JSON %q", got)
	}
	var anotherWriter bytes.Buffer
	slice5Destination(t, &anotherWriter)
	if got := read(false, false); got != wantPlain {
		t.Errorf("plain batch %q", got)
	}
	if got := read(true, false); got != wantPlain {
		t.Errorf("plain follow %q", got)
	}
}
