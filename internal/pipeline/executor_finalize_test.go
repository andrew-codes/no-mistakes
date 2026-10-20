package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// finalizingStep records the run state a RunFinalizer sees: the executor must
// call it only after the terminal status is durable and every step has its
// final state, since that is the state the artifact has to describe.
type finalizingStep struct {
	*mockStep
	calls     int
	seenRun   types.RunStatus
	seenSteps []types.StepStatus
}

func (f *finalizingStep) FinalizeRun(sctx *StepContext) error {
	f.calls++
	f.seenRun = sctx.Run.Status
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return err
	}
	for _, sr := range steps {
		f.seenSteps = append(f.seenSteps, sr.Status)
	}
	return nil
}

func TestExecutor_FinalizesAfterACompletedRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	fin := &finalizingStep{mockStep: newPassStep(types.StepPR)}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview), fin}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if fin.calls != 1 || fin.seenRun != types.RunCompleted {
		t.Fatalf("finalizer calls=%d run status=%q, want one call on a completed run", fin.calls, fin.seenRun)
	}
	for _, s := range fin.seenSteps {
		if s != types.StepStatusCompleted {
			t.Fatalf("finalizer saw a step still %q, want every step settled: %v", s, fin.seenSteps)
		}
	}
}

func TestExecutor_FinalizesAfterAFailedRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	fin := &finalizingStep{mockStep: newPassStep(types.StepPR)}
	exec := NewExecutor(database, p, nil, nil, []Step{fin, newFailStep(types.StepCI, errors.New("ci blew up"))}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("expected the failing step to fail the run")
	}
	if fin.calls != 1 || fin.seenRun != types.RunFailed {
		t.Fatalf("finalizer calls=%d run status=%q, want one call on a failed run", fin.calls, fin.seenRun)
	}
}
