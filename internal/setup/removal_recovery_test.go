package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"berthy/internal/scope"
)

func TestRemovalRetryKeepsReplacementKeyAfterPeerTokenChange(t *testing.T) {
	r, opts := newRemovalRunner(t)
	r.restartCode = 1
	if _, failure := runRemoval(r, opts, false, false); !strings.Contains(failure, "previous one was put back") {
		t.Fatalf("initial rollback = %q", failure)
	}
	record := bytes.Clone(r.record)
	r.live = bytes.Replace(r.live, []byte(strings.Repeat("2", 64)), []byte(strings.Repeat("3", 64)), 1)
	live := bytes.Clone(r.live)
	r.paths = strings.Replace(r.paths, "12 35", "12 99", 1)
	r.mutations, r.restartCode = nil, 0
	s := &Session{R: r, Opts: opts}
	var failure string
	Run(context.Background(), s, RemovalSteps(false), func(res StepResult) {
		if res.Status == StatusFailed {
			failure = res.Detail
		}
	})
	if !strings.Contains(failure, "file identities changed") ||
		!strings.Contains(failure, removalRecordPath(scope.ID{Project: opts.Project, Environment: opts.Env})) ||
		!strings.Contains(failure, "move the record aside") || r.stateClean ||
		!bytes.Equal(record, r.record) || !bytes.Equal(live, r.live) {
		t.Fatalf("retry = %q, removed files = %t, mutations = %v", failure, r.stateClean, r.mutations)
	}
	r.checkMutations("")
}

func TestRemovalRetryLeavesOrphanedGuardOnFailedStateProbe(t *testing.T) {
	r, opts := newRemovalRunner(t)
	r.failTimer = true
	if _, failure := runRemoval(r, opts, false, false); !strings.Contains(failure, "timer registration failed") {
		t.Fatalf("initial failure = %q", failure)
	}
	if r.guard != "pending" {
		t.Fatalf("initial guard = %q, want pending", r.guard)
	}
	r.failTimer, r.mutations, r.appliedAppsCode = false, nil, 2
	_, failure := runRemoval(r, opts, false, false)
	if !strings.Contains(failure, "could not verify applied apps") || r.guard != "pending" {
		t.Fatalf("retry = %q, guard = %q, mutations = %v", failure, r.guard, r.mutations)
	}
	r.checkMutations("")
}

func TestRemovalRecoversWhenTimerRegistrationFails(t *testing.T) {
	r, opts := newRemovalRunner(t)
	original := bytes.Clone(r.live)
	r.failTimer = true
	if _, failure := runRemoval(r, opts, false, false); !strings.Contains(failure, "timer registration failed") {
		t.Fatalf("initial failure = %q", failure)
	}
	if r.guard != "pending" || !bytes.Equal(r.live, original) {
		t.Fatal("the failed timer registration must leave the original config live with an orphaned guard")
	}
	r.failTimer, r.mutations = false, nil
	r.restoreScheduled = true
	if _, failure := runRemoval(r, opts, false, false); !strings.Contains(failure, "guard is still running") {
		t.Fatalf("registered timer refusal = %q", failure)
	}
	r.checkMutations("")
	r.restoreScheduled = false
	if _, failure := runRemoval(r, opts, false, true); failure != "" || r.guard != "pending" {
		t.Fatalf("orphan preview = %q, guard %q", failure, r.guard)
	}
	r.checkMutations("")
	s, failure := runRemoval(r, opts, false, false)
	if failure != "" || s.removal == nil || !s.removal.Activated {
		t.Fatalf("orphan retry = %q", failure)
	}
	r.checkMutations("stage,swap,restart,record:confirmed")
}

func TestRemovalReplacesOnlyStaleUnactivatedRecord(t *testing.T) {
	t.Run("unactivated pair still hosted", func(t *testing.T) {
		r, opts := newRemovalRunner(t)
		r.restartCode = 1
		if _, failure := runRemoval(r, opts, false, false); !strings.Contains(failure, "previous one was put back") {
			t.Fatalf("initial rollback = %q", failure)
		}
		var original removal
		if err := json.Unmarshal(r.record, &original); err != nil {
			t.Fatal(err)
		}
		r.live = bytes.Replace(r.live, []byte(strings.Repeat("2", 64)), []byte(strings.Repeat("3", 64)), 1)
		freshSHA := configDigest(r.live)
		r.mutations, r.restartCode = nil, 0
		s, failure := runRemoval(r, opts, true, false)
		if failure != "" || s.removal == nil || s.removal.OriginalSHA != freshSHA || s.removal.Archive != "" || !sameRemovalFiles(*s.removal, original) {
			t.Fatalf("fresh removal = %q, record %+v", failure, s.removal)
		}
		r.checkMutations("record:pending,stage,swap,restart,record:confirmed")
	})
	t.Run("activated", func(t *testing.T) {
		r, opts := newRemovalRunner(t)
		if _, failure := runRemoval(r, opts, false, false); failure != "" {
			t.Fatal(failure)
		}
		var rec removal
		if err := json.Unmarshal(r.record, &rec); err != nil {
			t.Fatal(err)
		}
		before := bytes.Clone(r.record)
		r.live = append(r.live, '\n')
		r.mutations = nil
		_, failure := runRemoval(r, opts, false, false)
		if !strings.Contains(failure, removalRecordPath(rec.ID)) || !strings.Contains(failure, "finish cleanup manually") || !bytes.Equal(before, r.record) {
			t.Fatalf("changed activated generation = %q", failure)
		}
		r.checkMutations("")
	})
}
