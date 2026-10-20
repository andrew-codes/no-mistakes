package steps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The PR description is composed once, by the PR step, which runs BEFORE the CI
// step and any CI repair rounds. Everything the body says about the run (test
// outcome, open findings, step statuses) is therefore a snapshot of that
// moment: later the tests pass, a repair lands, the PR leaves draft, and the
// description keeps claiming the old state. Only the attestation's head is
// restamped on repair; nothing rewrote the rest.
//
// FinalizeRun is the missing last write. The executor calls it once the run is
// terminal and every step's final state is in the database; it recomposes the
// generated sections from that state and leaves the author's narrative alone.

var _ pipeline.RunFinalizer = (*PRStep)(nil)

// FinalizeRun refreshes the description of the PR this run published so it
// reflects the run's final state. It never creates a PR, never re-drafts the
// narrative (no agent call), and only touches a PR whose body this run's PR
// step wrote.
func (s *PRStep) FinalizeRun(sctx *pipeline.StepContext) error {
	host, pr, err := s.openPublishedPR(sctx)
	if err != nil || pr == nil {
		return err
	}
	refreshErr := s.refreshDescription(sctx, host, pr)
	// Readiness is decided last, after the description is current: a PR whose
	// description could not be refreshed is not published as ready.
	return errors.Join(refreshErr, syncPRDraftState(sctx, host, pr, refreshErr == nil))
}

// openPublishedPR resolves the PR this run's PR step wrote, or nil when there
// is none to finalize.
func (s *PRStep) openPublishedPR(sctx *pipeline.StepContext) (scm.Host, *scm.PR, error) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("read steps: %w", err)
	}
	published := false
	for _, sr := range steps {
		if sr.StepName == types.StepPR && sr.Status == types.StepStatusCompleted {
			published = true
		}
	}
	if !published {
		// This run never wrote the body (skipped, failed first, or no forge),
		// so whatever is there belongs to an earlier run.
		return nil, nil, nil
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	if branch == effectivePRBaseBranch(sctx) {
		return nil, nil, nil
	}
	provider := resolvedProvider(sctx)
	host, _ := buildHost(sctx, provider)
	if host == nil {
		return nil, nil, nil
	}
	if err := host.Available(sctx.Ctx); err != nil {
		if pluginContractBroken(err) {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	found, err := host.FindPR(sctx.Ctx, branch, "")
	if err != nil {
		return nil, nil, fmt.Errorf("find pull request: %w", err)
	}
	pr, err := bindExistingPR(sctx, host, found)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve pull request: %w", err)
	}
	return host, pr, nil
}

// refreshDescription recomposes the generated sections of pr's description.
func (s *PRStep) refreshDescription(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return fmt.Errorf("provider cannot read pull request content")
	}
	provider := resolvedProvider(sctx)
	live, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("read pull request: %w", err)
	}
	if err := claimClosingIssueRefs(sctx, host, provider); err != nil {
		return err
	}
	bodyLimit := scm.HostMaxPRBodyChars(host)

	if hasPRAppendixMarkers(live.Body) {
		if _, err := parsePROwnedBody(live.Body); err != nil {
			return err
		}
		appendix, err := s.buildPRAppendixWith(sctx, provider, true)
		if err != nil {
			return err
		}
		return updateOwnedPR(sctx, host, pr, live, "", "", appendix, bodyLimit)
	}

	narrative := neutralizeAttestationMarkers(provider, stripGeneratedSections(live.Body))
	pipelineMD, riskLine, testingMD := s.buildPipelineSectionWith(sctx, provider, false, true)
	body := assembleDraftPRBody(sctx, narrative, riskLine, testingMD, pipelineMD, bodyLimit, provider)
	content := redactPRContent(prContent{Body: body})
	if err := verifyClosingIssuesInBody(content.Body, sctx); err != nil {
		return err
	}
	if content.Body == live.Body {
		return nil
	}
	// Body only: a concurrent title edit stays untouched.
	if _, err := host.UpdatePR(sctx.Ctx, pr, scm.PRContent(content)); err != nil {
		return fmt.Errorf("update pull request description: %w", err)
	}
	return nil
}

// finalRunOutcomeLine states how the run ended, in the terms a reviewer needs:
// whether it passed, and if not where it stopped.
func finalRunOutcomeLine(run *db.Run, steps []*db.StepResult) string {
	head := run.HeadSHA
	if len(head) > 8 {
		head = head[:8]
	}
	switch run.Status {
	case types.RunCompleted:
		return fmt.Sprintf("**Run outcome:** ✅ pipeline passed on `%s`.", head)
	case types.RunFailed:
		where := ""
		for _, sr := range steps {
			if sr.Status == types.StepStatusFailed {
				where = " at " + stepDisplayName(sr.StepName)
				break
			}
		}
		reason := ""
		if run.Error != nil {
			if msg := sanitizePromptText(*run.Error); msg != "" {
				if len(msg) > 200 {
					msg = msg[:utf8BoundaryBefore(msg, 200)] + "..."
				}
				reason = ": " + msg
			}
		}
		return fmt.Sprintf("**Run outcome:** ❌ pipeline failed%s on `%s`%s", where, head, reason)
	}
	return ""
}
