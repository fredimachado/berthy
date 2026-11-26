package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		{"tokens", "NAME\tSCOPE\tAGENT\tORIGIN\tCREATED\tEXPIRES\tLAST USED\n界\tadmin\tno\t\t-\tnever\tnever\né\tadmin\tno\t\t-\tnever\tnever\n", func(o, e io.Writer) int {
			return printTokens(o, e, []byte(`[{"name":"界","scope":"admin"},{"name":"é","scope":"admin"}]`))
		}},
		{"backups", "BACKUP\tAPP\tVOLUME\tSIZE\tFINISHED\tSHA256\tKEY\n界\tapp\tcommand\t0 B\t2026-01-01T00:00:00Z\t\t\né\tapp\tcommand\t0 B\t2026-01-01T00:00:00Z\t\t\n", func(o, e io.Writer) int {
			return printBackups(o, e, []byte(`[{"backup":"界","app":"app","finished":"2026-01-01T00:00:00Z"},{"backup":"é","app":"app","finished":"2026-01-01T00:00:00Z"}]`), false)
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
			out.Reset()
			slice5Destination(t, &out)
			if got := tc.run(&out, &errs); got != 0 {
				t.Fatalf("color exit %d: %s", got, errs.String())
			}
			if !strings.Contains(out.String(), "\x1b[") {
				t.Errorf("missing color: %q", out.String())
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
			if tc.name == "jobs" && !strings.Contains(out.String(), "\x1b[31mfailed\x1b[0m: detail") {
				t.Errorf("job detail styling: %q", out.String())
			}
			if got := tc.run(slice6FailWriter{}, &errs); got != 1 {
				t.Errorf("failed writer exit %d", got)
			}
		})
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
}
