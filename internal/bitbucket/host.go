package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Host implements scm.Host for Bitbucket using the twg-backed Client.
type Host struct {
	client       *Client
	repo         RepoRef
	cliAvailable func() bool
}

// NewHost builds a Host from a twg-backed client and a parsed repository
// reference. cliAvailable reports whether the twg binary is resolvable on
// the caller's PATH (possibly overridden by env); when nil, availability is
// determined solely by the doctor check in Available.
func NewHost(client *Client, repo RepoRef, cliAvailable func() bool) *Host {
	return &Host{client: client, repo: repo, cliAvailable: cliAvailable}
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
	pr, err := h.client.CreatePR(ctx, h.repo, branch, base, content.Title, content.Body)
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
			Name:   statusName(status),
			Bucket: statusBucket(status.State),
		})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(_ context.Context, _ *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
}

// FetchFailedCheckLogs finds the pipeline behind a failing check by reading
// the check's status URL (Bitbucket Pipelines build statuses always link to
// their pipeline result: .../pipelines/results/<uuid>) and fetches its
// failed step's log directly. twg has no "list pipelines by commit" primitive,
// so this only covers checks whose status carries a result URL - the case
// every real caller hits, since failingNames always comes from a prior
// GetChecks pass over the same statuses.
func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, _ string, failingNames []string) (string, error) {
	if h.client == nil || len(failingNames) == 0 {
		return "", nil
	}
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return "", err
	}
	statuses, err := h.client.ListPRStatuses(ctx, h.repo, id)
	if err != nil {
		return "", nil
	}
	for _, uuid := range failedPipelineUUIDs(statuses, failingNames) {
		logOutput, err := h.client.GetFailedStepLog(ctx, h.repo, uuid)
		if err != nil || strings.TrimSpace(logOutput) == "" {
			continue
		}
		return strings.TrimSpace(logOutput), nil
	}
	return "", nil
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

func normalizePipelineUUID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.Trim(trimmed, "{}")
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(trimmed)
}

func pipelineUUIDFromStatusURL(raw string) string {
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
		uuid := fragment[idx+len("/results/"):]
		uuid = strings.TrimSpace(strings.SplitN(uuid, "?", 2)[0])
		uuid = strings.TrimSpace(strings.SplitN(uuid, "/", 2)[0])
		return normalizePipelineUUID(uuid)
	}
	return ""
}

// failedPipelineUUIDs returns the pipeline UUIDs (in stable order) behind the
// named failing statuses, derived from each status's result URL.
func failedPipelineUUIDs(statuses []CommitStatus, failingNames []string) []string {
	if len(failingNames) == 0 {
		return nil
	}
	failing := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			failing[trimmed] = struct{}{}
		}
	}
	if len(failing) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var uuids []string
	for _, status := range LatestStatuses(statuses) {
		if _, ok := failing[statusName(status)]; !ok {
			continue
		}
		uuid := pipelineUUIDFromStatusURL(status.URL)
		if uuid == "" {
			continue
		}
		if _, ok := seen[uuid]; ok {
			continue
		}
		seen[uuid] = struct{}{}
		uuids = append(uuids, uuid)
	}
	return uuids
}
