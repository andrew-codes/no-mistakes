package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// idleCIMonitorStep stands in for a CI monitor that is only watching an open
// PR: it blocks until its context ends, as the real poll loop does.
type idleCIMonitorStep struct {
	started chan struct{}
	calls   atomic.Int32
}

func (s *idleCIMonitorStep) Name() types.StepName { return types.StepCI }
func (s *idleCIMonitorStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if s.calls.Add(1) == 1 && s.started != nil {
		close(s.started)
	}
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

func monitoredRun(t *testing.T, database *db.DB, run *db.Run, withPR bool) {
	t.Helper()
	if withPR {
		url := "https://github.com/test/repo/pull/7"
		if err := database.UpdateRunPRURL(run.ID, url); err != nil {
			t.Fatal(err)
		}
		run.PRURL = &url
	}
}

func shutDownMonitor(t *testing.T, exec *Executor, run *db.Run, repo *db.Repo, step *idleCIMonitorStep) error {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	select {
	case <-step.started:
	case <-time.After(5 * time.Second):
		t.Fatal("CI monitor did not start")
	}
	cancel(ErrDaemonShutdown)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not stop after shutdown")
		return nil
	}
}

// A clean daemon stop reaches an idle CI monitor the way a crash does: the PR
// is still open, so the run keeps its record and the next start resumes it.
func TestExecutor_ShutdownLeavesAnIdleCIMonitorResumable(t *testing.T) {
	database, p, run, repo := setupTest(t)
	monitoredRun(t, database, run, true)
	step := &idleCIMonitorStep{started: make(chan struct{})}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	if err := shutDownMonitor(t, exec, run, repo, step); !errors.Is(err, ErrParkedAtShutdown) {
		t.Fatalf("Execute() error = %v, want ErrParkedAtShutdown", err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Fatalf("run after shutdown = %s, want running so the next start resumes it", got.Status)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != types.StepStatusRunning {
		t.Fatalf("CI step after shutdown = %+v, want running", results)
	}
	if _, err := ValidateRecoveredCIMonitor(database, got, []Step{step}); err != nil {
		t.Fatalf("shutdown record is not resumable: %v", err)
	}
}

// Without a PR there is nothing to keep watching: shutdown fails the step as
// it always did.
func TestExecutor_ShutdownWithoutAPRStillFailsTheCIStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	monitoredRun(t, database, run, false)
	step := &idleCIMonitorStep{started: make(chan struct{})}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	if err := shutDownMonitor(t, exec, run, repo, step); errors.Is(err, ErrParkedAtShutdown) {
		t.Fatalf("Execute() = %v, want a failure: no PR is open", err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", got.Status)
	}
}

// The record a shutdown leaves is what a fresh executor resumes: the CI step
// runs again, and the run finishes through the ordinary completion path.
func TestExecutor_ResumeCIMonitorRunsTheStepAgain(t *testing.T) {
	database, p, run, repo := setupTest(t)
	monitoredRun(t, database, run, true)
	first := &idleCIMonitorStep{started: make(chan struct{})}
	if err := shutDownMonitor(t, NewExecutor(database, p, nil, nil, []Step{first}, nil), run, repo, first); !errors.Is(err, ErrParkedAtShutdown) {
		t.Fatalf("Execute() error = %v, want ErrParkedAtShutdown", err)
	}
	recovered, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	second := &passingStep{name: types.StepCI}
	exec := NewExecutor(database, p, nil, nil, []Step{second}, nil)
	if err := exec.ResumeCIMonitor(context.Background(), recovered, repo, t.TempDir()); err != nil {
		t.Fatalf("ResumeCIMonitor() error = %v", err)
	}
	if second.calls.Load() != 1 {
		t.Fatalf("CI step ran %d times after the resume, want 1", second.calls.Load())
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s, want completed", got.Status)
	}
}

type passingStep struct {
	name  types.StepName
	calls atomic.Int32
}

func (s *passingStep) Name() types.StepName { return s.name }
func (s *passingStep) Execute(*StepContext) (*StepOutcome, error) {
	s.calls.Add(1)
	return &StepOutcome{}, nil
}

func TestValidateRecoveredCIMonitor_RefusesWhatIsNotAnIdleMonitor(t *testing.T) {
	seed := func(t *testing.T, mutate func(database *db.DB, run *db.Run, ci *db.StepResult)) (*db.DB, *db.Run) {
		database, _, run, _ := setupTest(t)
		monitoredRun(t, database, run, true)
		if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		run.Status = types.RunRunning
		ci, err := database.InsertStepResult(run.ID, types.StepCI)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(ci.ID); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(database, run, ci)
			refreshed, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			run = refreshed
		}
		return database, run
	}
	steps := []Step{&passingStep{name: types.StepCI}}

	t.Run("idle monitor", func(t *testing.T) {
		database, run := seed(t, nil)
		index, err := ValidateRecoveredCIMonitor(database, run, steps)
		if err != nil || index != 0 {
			t.Fatalf("ValidateRecoveredCIMonitor() = %d, %v; want 0, nil", index, err)
		}
	})
	for name, mutate := range map[string]func(*db.DB, *db.Run, *db.StepResult){
		"parked at a gate": func(database *db.DB, run *db.Run, ci *db.StepResult) {
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
		},
		"agent recorded": func(database *db.DB, _ *db.Run, ci *db.StepResult) {
			pid := 99
			if err := database.SetStepAgentActivity(ci.ID, "agent started", &pid); err != nil {
				t.Fatal(err)
			}
		},
		"fix round in flight": func(database *db.DB, _ *db.Run, ci *db.StepResult) {
			if err := database.StartStepFixRound(ci.ID, 3); err != nil {
				t.Fatal(err)
			}
		},
		"run already terminal": func(database *db.DB, run *db.Run, _ *db.StepResult) {
			if err := database.UpdateRunStatus(run.ID, types.RunCIMonitorInterrupted); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			database, run := seed(t, mutate)
			if _, err := ValidateRecoveredCIMonitor(database, run, steps); err == nil {
				t.Fatal("ValidateRecoveredCIMonitor() accepted a run that is not an idle CI monitor")
			}
		})
	}
}
