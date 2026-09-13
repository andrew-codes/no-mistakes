package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/andrew-codes/no-mistakes/internal/scm"
)

// Host implements scm.Host for Bitbucket using the twg-backed Client.
type Host struct {
	client       *Client
	repo         RepoRef
	cliAvailable func() bool
	draft        bool // open created PRs as drafts ("--draft" on twg's create)
}

// NewHost builds a Host from a twg-backed client and a parsed repository
// reference. cliAvailable reports whether the twg binary is resolvable on
// the caller's PATH (possibly overridden by env); when nil, availability is
// determined solely by the doctor check in Available. When draft is true,
// created PRs are opened as drafts.
func NewHost(client *Client, repo RepoRef, cliAvailable func() bool, draft bool) *Host {
	return &Host{client: client, repo: repo, cliAvailable: cliAvailable, draft: draft}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderBitbucket }

// Capabilities reports Bitbucket's feature matrix. Bitbucket's REST API
// does not expose a reliable merge-conflict probe, so MergeableState is off.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: false, FailedCheckLogs: true}
}

// doctorPayload mirrors the subset of `twg doctor --output json`'s data that
// reports Bitbucket-specific auth resolution.
type doctorPayload struct {
	Bitbucket struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"bitbucket"`
}

func (h *Host) Available(ctx context.Context) error {
	if h.client == nil {
		return errors.New("bitbucket client is not configured")
	}
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("twg CLI is not installed")
	}
	data, err := h.client.run(ctx, "doctor")
	if err != nil {
		return fmt.Errorf("twg doctor: %w", err)
	}
	var doctor doctorPayload
	if err := json.Unmarshal(data, &doctor); err != nil {
		return fmt.Errorf("twg doctor: decode response: %w", err)
	}
	if !doctor.Bitbucket.OK {
		msg := strings.TrimSpace(doctor.Bitbucket.Message)
		if msg == "" {
			msg = "twg is not authenticated with Bitbucket"
		}
		return errors.New(msg)
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	pr, err := h.client.FindOpenPRBySourceBranch(ctx, h.repo, branch, base)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, nil
	}
	return h.toPR(pr), nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	pr, err := h.client.CreatePR(ctx, h.repo, branch, base, content.Title, content.Body, h.draft)
	if err != nil {
		return nil, err
	}
	return h.toPR(pr), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return nil, fmt.Errorf("invalid Bitbucket PR number %q: %w", pr.Number, err)
	}
	updated, err := h.client.UpdatePR(ctx, h.repo, id, content.Title, content.Body)
	if err != nil {
		return nil, err
	}
	return h.toPR(updated), nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return "", err
	}
	got, err := h.client.GetPR(ctx, h.repo, id)
	if err != nil {
		return "", err
	}
	if got == nil {
		return "", nil
	}
	return normalizePRState(got.State), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return nil, err
	}
	statuses, err := h.client.ListPRStatuses(ctx, h.repo, id)
	if err != nil {
		return nil, err
	}
	statuses = LatestStatuses(statuses)
	checks := make([]scm.Check, 0, len(statuses))
	for _, status := range statuses {
		checks = append(checks, scm.Check{
			Name:        statusName(status),
			ProviderID:  statusProviderID(status),
			Bucket:      statusBucket(status.State),
			ExecutionID: pipelineBuildNumberFromStatusURL(status.URL),
		})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(_ context.Context, _ *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
}

// FetchFailedCheckLogs finds the pipeline behind each named failing check by
// reading the check's status URL (Bitbucket Pipelines build statuses always
// link to their pipeline result, whose trailing path segment is the build
// number: .../pipelines/results/<build-number>) and fetches its failed
// step's log directly via twg, which accepts a pipeline build number, UUID,
// or result URL interchangeably as --pipeline.
func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	targets := make([]scm.CheckTarget, 0, len(failingNames))
	for _, name := range failingNames {
		targets = append(targets, scm.CheckTarget{Name: name})
	}
	logs, err := h.FetchFailedCheckTargetLogs(ctx, pr, branch, headSHA, targets)
	if err != nil {
		return "", err
	}
	return scm.CombineFailedCheckLogs(logs)
}

// FetchFailedCheckTargetLogs implements scm.TargetedFailedCheckLogsHost,
// fetching the failed-step log for exactly the selected checks rather than
// every failing check on the PR.
func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _ string, _ string, selected []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if h.client == nil {
		return nil, errors.New("bitbucket client is not configured")
	}
	if len(selected) == 0 {
		return nil, nil
	}
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return nil, err
	}
	statuses, err := h.client.ListPRStatuses(ctx, h.repo, id)
	if err != nil {
		return nil, fmt.Errorf("resolve selected Bitbucket checks: %w", err)
	}
	results := make([]scm.FailedCheckLog, 0, len(selected))
	for _, target := range selected {
		result := scm.FailedCheckLog{Target: target}
		buildNumbers, resolveErr := failedPipelineBuildNumberTargets(statuses, []scm.CheckTarget{target})
		if resolveErr != nil {
			result.Err = resolveErr
			results = append(results, result)
			continue
		}
		var outputs []string
		var logErrors []error
		for buildNumber := range buildNumbers {
			logOutput, err := h.client.GetFailedStepLog(ctx, h.repo, buildNumber)
			if err != nil {
				logErrors = append(logErrors, fmt.Errorf("fetch Bitbucket pipeline %s log: %w", buildNumber, err))
				continue
			}
			if log := strings.TrimSpace(logOutput); log != "" {
				outputs = append(outputs, log)
			}
		}
		result.Output = strings.Join(outputs, "\n\n")
		result.Err = errors.Join(logErrors...)
		results = append(results, result)
	}
	return results, nil
}

func (h *Host) toPR(pr *PullRequest) *scm.PR {
	if pr == nil {
		return nil
	}
	return &scm.PR{
		Number: strconv.Itoa(pr.ID),
		URL:    prURL(h.repo, pr.ID, pr.URL),
	}
}

func prURL(repo RepoRef, prID int, rawURL string) string {
	if url := strings.TrimSpace(rawURL); url != "" {
		return url
	}
	if prID <= 0 || strings.TrimSpace(repo.Workspace) == "" || strings.TrimSpace(repo.RepoSlug) == "" {
		return ""
	}
	return fmt.Sprintf("https://bitbucket.org/%s/%s/pull-requests/%d", repo.Workspace, repo.RepoSlug, prID)
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "DECLINED", "CLOSED", "SUPERSEDED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

// LatestStatuses keeps only the newest status per unique key/name.
// Exported because legacy step code still calls it by name during the migration.
func LatestStatuses(statuses []CommitStatus) []CommitStatus {
	latest := make([]CommitStatus, 0, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		id := strings.TrimSpace(status.Key)
		if id == "" {
			id = statusName(status)
		}
		if id == "" {
			latest = append(latest, status)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		latest = append(latest, status)
	}
	return latest
}

func statusName(status CommitStatus) string {
	name := strings.TrimSpace(status.Name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(status.Key)
}

// statusProviderID derives a stable identity for a status beyond its display
// name, so same-named statuses (e.g. a matrix job repeated across platforms)
// can be told apart by callers matching on scm.CheckTarget.ProviderID.
func statusProviderID(status CommitStatus) string {
	if key := strings.TrimSpace(status.Key); key != "" {
		return "bitbucket-status:" + key
	}
	if buildNumber := pipelineBuildNumberFromStatusURL(status.URL); buildNumber != "" {
		return "bitbucket-pipeline-build:" + buildNumber
	}
	return ""
}

func statusBucket(state string) scm.CheckBucket {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESSFUL", "SUCCESS":
		return scm.CheckBucketPass
	case "FAILED", "FAILURE", "ERROR":
		return scm.CheckBucketFail
	case "STOPPED":
		return scm.CheckBucketCancel
	case "INPROGRESS", "IN_PROGRESS", "PENDING":
		return scm.CheckBucketPending
	default:
		return ""
	}
}

// pipelineBuildNumberFromStatusURL extracts the pipeline build number from a
// Bitbucket Pipelines status URL. The trailing "/results/<N>" path segment
// (in either the URL path or its fragment) is the build number Bitbucket's
// own web UI links to, not a UUID; twg's --pipeline flag accepts this number
// directly.
func pipelineBuildNumberFromStatusURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	fragments := []string{parsed.Fragment, parsed.Path}
	for _, fragment := range fragments {
		idx := strings.LastIndex(fragment, "/results/")
		if idx < 0 {
			continue
		}
		buildNumber := fragment[idx+len("/results/"):]
		buildNumber = strings.TrimSpace(strings.SplitN(buildNumber, "?", 2)[0])
		buildNumber = strings.TrimSpace(strings.SplitN(buildNumber, "/", 2)[0])
		buildNumber = strings.Trim(buildNumber, "{}")
		if number, err := strconv.Atoi(buildNumber); err == nil && number > 0 {
			return strconv.Itoa(number)
		}
		return ""
	}
	return ""
}

// failedPipelineBuildNumberTargets resolves each selected check target to the
// pipeline build number(s) behind it, matching by ProviderID when the target
// carries one (so two same-named checks are never conflated) and falling
// back to name otherwise. It fails closed - a selection that cannot be
// matched to a failed status with a resolvable pipeline identity is an error,
// never a silently empty result.
func failedPipelineBuildNumberTargets(statuses []CommitStatus, selected []scm.CheckTarget) (map[string]struct{}, error) {
	latest := LatestStatuses(statuses)
	targets := make(map[string]struct{}, len(selected))
	var resolveErrors []error
	for _, target := range selected {
		matched := false
		for _, status := range latest {
			if statusBucket(status.State) != scm.CheckBucketFail {
				continue
			}
			if target.ProviderID != "" {
				if statusProviderID(status) != target.ProviderID {
					continue
				}
			} else if statusName(status) != strings.TrimSpace(target.Name) {
				continue
			}
			matched = true
			if buildNumber := pipelineBuildNumberFromStatusURL(status.URL); buildNumber != "" {
				targets[buildNumber] = struct{}{}
			} else {
				resolveErrors = append(resolveErrors, fmt.Errorf("selected Bitbucket check %q has no pipeline identity", statusName(status)))
			}
		}
		if !matched {
			identity := target.ProviderID
			if identity == "" {
				identity = target.Name
			}
			resolveErrors = append(resolveErrors, fmt.Errorf("selected Bitbucket check %q was not found", identity))
		}
	}
	if err := errors.Join(resolveErrors...); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("no selected Bitbucket checks resolved to pipelines")
	}
	return targets, nil
}

var _ scm.TargetedFailedCheckLogsHost = (*Host)(nil)
