package db

import (
	"strings"
	"testing"
	"time"
)

func TestUpdateRunIntent_RoundTrip(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/intent", "git@github.com:user/intent.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	got, _ := d.GetRun(run.ID)
	if got.Intent != nil || got.IntentSource != nil || got.IntentSessionID != nil || got.IntentScore != nil {
		t.Fatalf("expected nil intent fields on fresh run, got %+v", got)
	}

	if err := d.UpdateRunIntent(run.ID, RunIntent{
		Summary:   "user wanted to add foo",
		Source:    "claude",
		SessionID: "abc-123",
		Score:     0.85,
	}); err != nil {
		t.Fatalf("update intent: %v", err)
	}

	got, _ = d.GetRun(run.ID)
	if got.Intent == nil || *got.Intent != "user wanted to add foo" {
		t.Errorf("intent = %v, want %q", got.Intent, "user wanted to add foo")
	}
	if got.IntentSource == nil || *got.IntentSource != "claude" {
		t.Errorf("intent source = %v, want claude", got.IntentSource)
	}
	if got.IntentSessionID == nil || *got.IntentSessionID != "abc-123" {
		t.Errorf("intent session = %v, want abc-123", got.IntentSessionID)
	}
	if got.IntentScore == nil || *got.IntentScore != 0.85 {
		t.Errorf("intent score = %v, want 0.85", got.IntentScore)
	}
}

func TestRunIntentCheckIsPinnedAndImmutable(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/intent-check", "git@github.com:user/intent-check.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	const policy = `{"mode":"reject","terms":["private"]}`
	if err := d.SetRunIntentCheck(run.ID, policy); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.IntentCheckJSON == nil || *got.IntentCheckJSON != policy {
		t.Fatalf("pinned intent policy = %v, err = %v", got.IntentCheckJSON, err)
	}
	if err := d.SetRunIntentCheck(run.ID, `{"mode":"off"}`); err == nil {
		t.Fatal("intent policy was overwritten")
	}
}

func TestIntentCache_PutGet(t *testing.T) {
	d := openTestDB(t)

	got, err := d.GetIntentCache("missing")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing key, got %+v", got)
	}

	if err := d.PutIntentCache(IntentCacheEntry{
		CacheKey:  "k1",
		Summary:   "do the thing",
		AgentName: "claude",
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err = d.GetIntentCache("k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Summary != "do the thing" || got.AgentName != "claude" {
		t.Fatalf("got %+v", got)
	}
	if got.CreatedAt == 0 {
		t.Errorf("expected created_at populated")
	}

	// Replace.
	if err := d.PutIntentCache(IntentCacheEntry{
		CacheKey:  "k1",
		Summary:   "do the new thing",
		AgentName: "claude",
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = d.GetIntentCache("k1")
	if got.Summary != "do the new thing" {
		t.Errorf("want replaced summary, got %q", got.Summary)
	}
}

func TestIntentCache_Cleanup(t *testing.T) {
	d := openTestDB(t)

	now := time.Now().Unix()
	d.PutIntentCache(IntentCacheEntry{CacheKey: "old", Summary: "x", AgentName: "claude", SessionID: "s", CreatedAt: now - int64((40 * 24 * time.Hour).Seconds())})
	d.PutIntentCache(IntentCacheEntry{CacheKey: "new", Summary: "x", AgentName: "claude", SessionID: "s", CreatedAt: now})

	deleted, err := d.CleanupOldIntentCache(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if got, _ := d.GetIntentCache("old"); got != nil {
		t.Errorf("expected old entry gone")
	}
	if got, _ := d.GetIntentCache("new"); got == nil {
		t.Errorf("expected new entry retained")
	}
}

func TestGetOriginalPRIntent(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/work", "https://github.com/o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.InsertRepo("/tmp/other", "https://github.com/o/other", "main")
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://github.com/o/r/pull/7"
	record := func(repoID, intent, prURL string) {
		t.Helper()
		run, err := d.InsertRun(repoID, "refs/heads/feature", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if intent != "" {
			if err := d.UpdateRunIntent(run.ID, RunIntent{Summary: intent, Source: RunIntentSourceAgent}); err != nil {
				t.Fatal(err)
			}
		}
		if prURL != "" {
			if err := d.UpdateRunPRURL(run.ID, prURL); err != nil {
				t.Fatal(err)
			}
		}
	}

	if got, err := d.GetOriginalPRIntent(repo.ID, url); err != nil || got != nil {
		t.Fatalf("no runs: got %+v, %v", got, err)
	}
	record(repo.ID, "", url)             // no intent recorded: skipped
	record(repo.ID, "original", url+"/") // first with an intent; trailing slash and
	record(repo.ID, "correction", url)   // later runs on the same PR do not displace it
	record(repo.ID, "other pr", url+"0") // a different PR
	record(other.ID, "other repo", url)  // the same URL in another repo
	got, err := d.GetOriginalPRIntent(repo.ID, strings.ToUpper(url))
	if err != nil || got == nil || got.Summary != "original" || got.Source != RunIntentSourceAgent {
		t.Fatalf("got %+v, %v; want the earliest intent bound to the PR", got, err)
	}
	if got, err := d.GetOriginalPRIntent(repo.ID, ""); err != nil || got != nil {
		t.Fatalf("empty URL: got %+v, %v", got, err)
	}
}
