package tui

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"berthy/internal/api"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
)

// statusModel is what the status line knows: the state of the connection to the agent, the lock, the pending approvals and
// when the data was last refreshed.
type statusModel struct {
	spin     spinner.Model
	spinning bool

	// inflight and fetched are the requests under way and when each was last sent.
	inflight map[string]bool
	fetched  map[string]time.Time

	unreachable bool
	failures    int
	retryAt     time.Time
	errs        map[string]string
	lastOK      time.Time

	lock         lockInfo
	lockKnown    bool
	pending      int
	pendingKnown bool
	infoKnown    bool
}

func newStatusModel(st styles) statusModel {
	return statusModel{
		spin:     spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(st.accent)),
		inflight: map[string]bool{},
		fetched:  map[string]time.Time{},
		errs:     map[string]string{},
	}
}

// busy is true while a request is under way.
func (s *statusModel) busy() bool { return len(s.inflight) > 0 }

// firstError is the message of the failed request that comes first by name, so the line does not flicker between them.
func (s *statusModel) firstError() string {
	if len(s.errs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.errs))
	for k := range s.errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return s.errs[keys[0]]
}

// backupSummary is what the apps say about their backups.
type backupSummary struct {
	// oldest is the age of the oldest of the newest successful backups.
	oldest   time.Duration
	has      bool
	failed   bool
	anyKnown bool
}

func summarizeBackups(apps []api.App, now time.Time) backupSummary {
	var b backupSummary
	for _, a := range apps {
		for _, bk := range a.Backups {
			b.anyKnown = true
			if bk.LastStatus == "failed" {
				b.failed = true
			}
			if bk.LastSuccess != nil {
				age := max(now.Sub(*bk.LastSuccess), 0)
				if !b.has || age > b.oldest {
					b.oldest = age
				}
				b.has = true
			}
		}
	}
	return b
}

// segment is one part of the status line.
type segment struct {
	text string
	// drop orders the parts that go first when the line is too narrow: the highest goes first.
	drop int
}

// view draws the status line, w cells wide.
func (s *statusModel) view(st styles, now time.Time, w int, backups backupSummary, spinning string, lastRefresh time.Time) string {
	var conn string
	switch {
	case s.unreachable:
		wait := max(int(s.retryAt.Sub(now).Round(time.Second)/time.Second), 0)
		retry := "retrying now"
		if wait > 0 {
			retry = "retrying in " + api.HumanAge(time.Duration(wait)*time.Second)
		}
		conn = st.err.Bold(true).Render(glyphFail + " agent unreachable — " + retry)
	case s.firstError() != "":
		conn = st.err.Render(glyphFail + " " + clip(sanitize(s.firstError()), 60))
	case s.busy():
		conn = spinning + st.ok.Render(" connected")
	case !s.lastOK.IsZero():
		conn = st.ok.Render(glyphHealthy + " connected")
	default:
		conn = spinning + st.muted.Render(" connecting")
	}
	segs := []segment{{conn, 99}}
	switch {
	case !s.lockKnown:
	case s.lock.Locked:
		text := glyphLock + " locked by " + sanitize(s.lock.Holder)
		if s.lock.Reason != "" {
			text += ": " + sanitize(s.lock.Reason)
		}
		segs = append(segs, segment{st.warn.Render(clip(text, 48)), 2})
	default:
		segs = append(segs, segment{st.muted.Render("unlocked"), 3})
	}
	switch {
	case backups.failed:
		segs = append(segs, segment{st.err.Render("backup failed"), 1})
	case backups.has:
		segs = append(segs, segment{st.muted.Render("backup " + api.HumanAge(backups.oldest) + " ago"), 4})
	}
	if s.pendingKnown {
		if s.pending > 0 {
			segs = append(segs, segment{st.warn.Bold(true).Render(strconv.Itoa(s.pending) + " pending"), 0})
		} else {
			segs = append(segs, segment{st.muted.Render("0 pending"), 5})
		}
	}
	right := ""
	if !lastRefresh.IsZero() {
		right = st.muted.Render("updated " + api.HumanAge(max(now.Sub(lastRefresh), 0)) + " ago")
	}
	sep := st.faint.Render(" │ ")
	for {
		parts := make([]string, len(segs))
		for i, sg := range segs {
			parts[i] = sg.text
		}
		line := " " + strings.Join(parts, sep)
		if lipgloss.Width(line)+lipgloss.Width(right)+1 <= w || len(segs) == 1 {
			return between(line, right+" ", w)
		}
		// Too narrow: drop the part of the highest drop order first, then the clock.
		worst := 0
		for i, sg := range segs {
			if sg.drop > segs[worst].drop && sg.drop != 99 {
				worst = i
			}
		}
		if segs[worst].drop == 99 {
			return between(line, "", w)
		}
		segs = append(segs[:worst], segs[worst+1:]...)
	}
}
