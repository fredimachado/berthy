package manifest

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestReferenceTextOfOneSection(t *testing.T) {
	got, err := ReferenceText("backups")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"retention.keep", "volume", "destination", "before"} {
		if !strings.Contains(got, want) {
			t.Errorf("the backups reference does not mention %s:\n%s", want, got)
		}
	}
	for _, other := range []string{"access_key_secret", "bot_token_secret", "deploy_timeout", "otlp_endpoint", "expect_status"} {
		if strings.Contains(got, other) {
			t.Errorf("the backups reference holds %s, a key of another section", other)
		}
	}
	if strings.Contains(got, "`") || strings.Contains(got, "| ---") {
		t.Errorf("the terminal reference holds Markdown:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 110 {
			t.Errorf("line of %d characters: %q", len(line), line)
		}
	}
}

func TestReferenceTextOfEverything(t *testing.T) {
	got, err := ReferenceText("")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range Keys {
		var section Section
		for _, s := range Sections {
			if s.Name == k.Section {
				section = s
			}
		}
		if rel := strings.TrimPrefix(k.Path, section.Prefix); !strings.Contains(got, rel) {
			t.Errorf("the reference does not show %s", k.Path)
		}
	}
	for _, s := range Sections {
		if !strings.Contains(got, s.Title) {
			t.Errorf("the reference does not show the section %q", s.Title)
		}
	}
}

func TestReferenceTextOfAnUnknownSectionListsTheSections(t *testing.T) {
	_, err := ReferenceText("nope")
	if err == nil {
		t.Fatal("an unknown section is accepted")
	}
	for _, s := range Sections {
		if !strings.Contains(err.Error(), s.Name) {
			t.Errorf("the error %q does not list the section %s", err, s.Name)
		}
	}
}

func TestReferenceMarkdownTables(t *testing.T) {
	got := ReferenceMarkdown()
	for _, s := range Sections {
		if !strings.Contains(got, "#### ") || !strings.Contains(got, s.Title) {
			t.Errorf("no heading for the section %q", s.Title)
		}
	}
	if !strings.Contains(got, "| Key | Type | Default | Meaning |") || !strings.Contains(got, "| Key | Applies to | Type | Default | Meaning |") {
		t.Errorf("no table header:\n%s", got)
	}
	cols := 0
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "|") {
			cols = 0
			continue
		}
		// A cell may hold a pipe as "\|". Every other pipe ends a cell.
		n := strings.Count(strings.ReplaceAll(line, `\|`, ""), "|")
		if cols == 0 {
			cols = n
		}
		if n != cols {
			t.Errorf("a row has %d pipes where its table's header has %d: %s", n, cols, line)
		}
	}
	if strings.HasSuffix(got, "\n\n") || !strings.HasSuffix(got, "\n") {
		t.Errorf("the reference ends with %q", got[max(0, len(got)-5):])
	}
	if strings.Contains(got, "\r") {
		t.Error("the Markdown holds a carriage return")
	}
}

func TestSectionNames(t *testing.T) {
	names := SectionNames()
	for _, want := range []string{"top", "apps", "backups"} {
		if !slices.Contains(names, want) {
			t.Errorf("%v does not name %s", names, want)
		}
	}
}

func TestUpdateGuideReplacesOnlyTheBlock(t *testing.T) {
	doc := "intro\n" + GuideBegin + "\nold\n" + GuideEnd + "\noutro\n"
	got, err := UpdateGuide(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "intro\n"+GuideBegin+"\n") || !strings.HasSuffix(got, GuideEnd+"\noutro\n") {
		t.Errorf("the text around the block changed:\n%s", got)
	}
	if strings.Contains(got, "STALE-BLOCK") || !strings.Contains(got, ReferenceMarkdown()) {
		t.Errorf("the block is not the reference:\n%s", got)
	}
	again, err := UpdateGuide(got)
	if err != nil || again != got {
		t.Errorf("a second update changed the text (err %v)", err)
	}
	crlf, err := UpdateGuide(strings.ReplaceAll(doc, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ReplaceAll(crlf, "\r\n", ""), "\n") || !strings.Contains(crlf, "\r\n") {
		t.Error("an update of a document with CRLF line endings does not keep them")
	}
	if strings.ReplaceAll(crlf, "\r\n", "\n") != got {
		t.Error("an update of a document with CRLF line endings differs from one with LF, apart from the line endings")
	}
}

func TestUpdateGuideNeedsBothMarkers(t *testing.T) {
	for name, doc := range map[string]string{
		"none":        "text\n",
		"no end":      GuideBegin + "\n",
		"no begin":    GuideEnd + "\n",
		"wrong order": GuideEnd + "\n" + GuideBegin + "\n",
	} {
		if _, err := UpdateGuide(doc); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestUserGuideReferenceIsCurrent keeps the reference of the user guide in step with Keys. The guide has CRLF line endings
// in a Windows checkout, which the comparison ignores.
func TestUserGuideReferenceIsCurrent(t *testing.T) {
	raw, err := os.ReadFile("../../docs/user-guide.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	want, err := UpdateGuide(doc)
	if err != nil {
		t.Fatal(err)
	}
	if want != doc {
		t.Error("the manifest reference in docs/user-guide.md is stale: run go generate ./internal/manifest")
	}
}
