package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// seedMonitoringCIRun records a run the daemon stopped on while its CI step was
// only watching an open PR: the PR is recorded, the worktree is checked out at
// the run's pushed head, and the CI step is running with no agent.
func seedMonitoringCIRun(t *testing.T, p *paths.Paths, d *db.DB, repoID string, mutate func(run *db.Run, ci *db.StepResult, worktree string, headSHA string)) (*db.Run, *db.StepResult, string) {
	t.Helper()
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+writeMockClaude(t, t.TempDir())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, headSHA := setupTestGitRepo(t, p, d, repoID)
	run, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	// A normal launch records the submitter identity; recovery refuses a run
	// without it.
	bindFixtureCommitIdentity(t, d, run.ID)
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(run.ID, "https://github.com/test/"+repoID+"/pull/42"); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(run, ci, worktree, headSHA)
	}
	return run, ci, worktree
}

func newCIResumeEnv(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return p, d
}

// A daemon restart used to end a monitored PR for good: the run became
// ci_monitor_interrupted and nothing watched the PR again, so feedback, a
// conflict or a merge-check failure that arrived afterwards was never handled.
func TestRecoverOnStartup_ResumesAnIdleCIMonitor(t *testing.T) {
	p, d := newCIResumeEnv(t)
	run, ci, _ := seedMonitoringCIRun(t, p, d, "resume-ci-monitor", nil)

	step := &mockPassStep{name: types.StepCI}
	runTestDaemon(t, p, d, func() []pipeline.Step { return []pipeline.Step{step} }, 3*time.Second)

	got := waitForRunTerminalState(t, d, run.ID)
	if got.Status == types.RunCIMonitorInterrupted {
		t.Fatalf("run ended as %s: the monitor was not resumed", got.Status)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s (error %v), want completed", got.Status, got.Error)
	}
	if step.execCnt.Load() != 1 {
		t.Fatalf("CI step executed %d times after the restart, want 1", step.execCnt.Load())
	}
	gotCI, err := d.GetStepResult(ci.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCI.Status != types.StepStatusCompleted {
		t.Fatalf("resumed CI step status = %s, want completed", gotCI.Status)
	}
}

// Resuming is only safe when nothing local can be lost or half applied; every
// other shape keeps ending as ci_monitor_interrupted, exactly as before.
func TestRecoverOnStartup_DoesNotResumeAnUnsafeCIMonitor(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, d *db.DB, run *db.Run, ci *db.StepResult, worktree, headSHA string)
	}{
		{"worktree head moved past the pushed head", func(t *testing.T, _ *db.DB, _ *db.Run, _ *db.StepResult, worktree, _ string) {
			if err := os.WriteFile(worktree+"/local-fix.txt", []byte("fix"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, worktree, "add", "-A")
			gitCmd(t, worktree, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "unpushed ci fix")
		}},
		{"worktree is gone", func(t *testing.T, _ *db.DB, _ *db.Run, _ *db.StepResult, worktree, _ string) {
			if err := os.RemoveAll(worktree); err != nil {
				t.Fatal(err)
			}
		}},
		{"an agent is still recorded on the step", func(t *testing.T, d *db.DB, _ *db.Run, ci *db.StepResult, _, _ string) {
			pid := 4242
			if err := d.SetStepAgentActivity(ci.ID, "agent started", &pid); err != nil {
				t.Fatal(err)
			}
		}},
		{"the step is mid fix round", func(t *testing.T, d *db.DB, _ *db.Run, ci *db.StepResult, _, _ string) {
			if err := d.StartStepFixRound(ci.ID, 3); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, d := newCIResumeEnv(t)
			run, _, _ := seedMonitoringCIRun(t, p, d, "no-resume-ci", func(run *db.Run, ci *db.StepResult, worktree, headSHA string) {
				tc.mutate(t, d, run, ci, worktree, headSHA)
			})

			step := &mockPassStep{name: types.StepCI}
			runTestDaemon(t, p, d, func() []pipeline.Step { return []pipeline.Step{step} }, 3*time.Second)

			got, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != types.RunCIMonitorInterrupted {
				t.Fatalf("run status = %s, want %s", got.Status, types.RunCIMonitorInterrupted)
			}
			if step.execCnt.Load() != 0 {
				t.Fatalf("CI step executed %d times, want 0", step.execCnt.Load())
			}
		})
	}
}
