package bitbucket

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// RerunCheck asks Bitbucket Pipelines to run the failed steps of the pipeline
// behind check again, for the same commit (`twg bb pipeline rerun-failed`), so a
// pipeline Bitbucket stopped rather than failed can be retried without a new
// push. The pipeline is identified by the build number of the check's status
// URL, the only pipeline identity a PR's commit statuses carry; a status that
// names no pipeline (a third-party status, or a URL this client cannot read)
// names nothing re-runnable, and the error says so rather than starting a fresh
// pipeline.
//
// Bitbucket reruns every eligible failed step of that pipeline, never one
// arbitrary step, and needs a token with pipeline write permission. Either
// refusal comes back as an error, which the CI step logs before escalating the
// check exactly as it would without reruns.
func (h *Host) RerunCheck(ctx context.Context, _ *scm.PR, check scm.Check) error {
	build := strings.TrimSpace(check.ExecutionID)
	if build == "" {
		build = pipelineBuildNumberFromStatusURL(check.Link)
	}
	if build == "" {
		return fmt.Errorf("check %q has no Bitbucket pipeline to re-run", check.Name)
	}
	args := []string{"bb", "pipeline", "rerun-failed", "--pipeline", build}
	args = append(args, h.repoArgs()...)
	out, err := h.runJSON(ctx, args)
	if err != nil {
		return fmt.Errorf("twg bb pipeline rerun-failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

var _ scm.CheckRerunner = (*Host)(nil)
