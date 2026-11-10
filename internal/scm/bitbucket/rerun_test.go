package bitbucket

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRerunCheckReRunsThePipelineBehindTheStatus(t *testing.T) {
	for name, check := range map[string]scm.Check{
		"build number from the execution ID":  {Name: "build", ExecutionID: "7"},
		"build number from the details link":  {Name: "build", Link: "https://bitbucket.org/ws/repo/pipelines/results/7"},
		"fragment style pipeline results URL": {Name: "build", Link: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/7"},
	} {
		t.Run(name, func(t *testing.T) {
			var rec []capturedCmd
			h := baseHost(&rec, bbTestResponse{stdout: `{"uuid":"{x}"}`})
			if err := h.RerunCheck(context.Background(), &scm.PR{Number: "42"}, check); err != nil {
				t.Fatalf("RerunCheck() = %v", err)
			}
			want := "bb pipeline rerun-failed --pipeline 7 --workspace ws --repo repo -o json --output-summary none"
			if len(rec) != 1 || !strings.Contains(joined(rec[0]), want) {
				t.Fatalf("commands = %v, want exactly %q", rec, want)
			}
		})
	}
}

// A status that names no pipeline has nothing to re-run: starting a fresh
// pipeline instead would run work nobody asked for.
func TestRerunCheckRefusesAStatusWithoutAPipeline(t *testing.T) {
	for name, check := range map[string]scm.Check{
		"no link":                  {Name: "build"},
		"third party status link":  {Name: "build", Link: "https://ci.example.com/job/12"},
		"results without a number": {Name: "build", Link: "https://bitbucket.org/ws/repo/pipelines/results/abc"},
	} {
		t.Run(name, func(t *testing.T) {
			var rec []capturedCmd
			h := baseHost(&rec, bbTestResponse{stdout: "{}"})
			err := h.RerunCheck(context.Background(), &scm.PR{Number: "42"}, check)
			if err == nil || !strings.Contains(err.Error(), "no Bitbucket pipeline to re-run") {
				t.Fatalf("RerunCheck() = %v, want a refusal", err)
			}
			if len(rec) != 0 {
				t.Fatalf("ran %d twg commands for a check with no pipeline, want none", len(rec))
			}
		})
	}
}

func TestRerunCheckReportsABitbucketRefusal(t *testing.T) {
	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stderr: "token lacks pipeline write permission", code: 1})
	err := h.RerunCheck(context.Background(), &scm.PR{Number: "42"}, scm.Check{Name: "build", ExecutionID: "7"})
	if err == nil || !strings.Contains(err.Error(), "token lacks pipeline write permission") {
		t.Fatalf("RerunCheck() = %v, want twg's own message", err)
	}
}

// The CI step reads a check's State and CompletedAt to decide whether a
// failure is a cancellation a rerun can replace, and whether a re-run has
// published its replacement yet.
func TestGetChecksReportsTheStateLinkAndCompletionTime(t *testing.T) {
	const prJSON = `{"id":42,"state":"OPEN","_statuses":[` +
		`{"name":"stopped","key":"a","state":"STOPPED","url":"https://bitbucket.org/ws/repo/pipelines/results/7","created_on":"2026-11-10T10:00:00Z","updated_on":"2026-11-10T10:05:00Z"},` +
		`{"name":"failed","key":"b","state":"FAILED","url":"https://bitbucket.org/ws/repo/pipelines/results/8","created_on":"2026-11-10T10:00:00Z"},` +
		`{"name":"passed","key":"c","state":"SUCCESSFUL"},` +
		`{"name":"running","key":"d","state":"INPROGRESS","created_on":"2026-11-10T10:00:00Z","updated_on":"2026-11-10T10:01:00Z"}` +
		`]}`
	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stdout: prJSON})
	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() = %v", err)
	}
	byName := map[string]scm.Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	stopped := byName["stopped"]
	if stopped.Bucket != scm.CheckBucketCancel || stopped.State != "CANCELLED" || stopped.ExecutionID != "7" || stopped.Link != "https://bitbucket.org/ws/repo/pipelines/results/7" {
		t.Fatalf("stopped check = %+v, want the cancel bucket, CANCELLED state, build 7 and its link", stopped)
	}
	if want := time.Date(2026, 11, 10, 10, 5, 0, 0, time.UTC); !stopped.CompletedAt.Equal(want) {
		t.Fatalf("stopped check completed at %v, want its last update %v", stopped.CompletedAt, want)
	}
	if failed := byName["failed"]; failed.State != "FAILED" || failed.Bucket != scm.CheckBucketFail || failed.CompletedAt.IsZero() {
		t.Fatalf("failed check = %+v, want FAILED, the fail bucket and a completion time", failed)
	}
	if passed := byName["passed"]; passed.State != "SUCCESSFUL" || !passed.CompletedAt.IsZero() {
		t.Fatalf("passed check = %+v, want SUCCESSFUL and no time when Bitbucket reported none", passed)
	}
	if running := byName["running"]; running.Bucket != scm.CheckBucketPending || !running.CompletedAt.IsZero() {
		t.Fatalf("running check = %+v, want pending with no completion time", running)
	}
}
