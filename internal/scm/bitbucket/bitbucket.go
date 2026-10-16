// Package bitbucket implements scm.Host backed by the twg CLI (Atlassian's
// Teamwork Graph CLI), which owns Bitbucket Cloud authentication itself the
// same way gh/glab/tea own auth for their providers. This replaces an earlier
// hand-rolled HTTP client that read a Bitbucket Cloud API token out of
// NO_MISTAKES_BITBUCKET_EMAIL/NO_MISTAKES_BITBUCKET_API_TOKEN: those variables
// lived in the daemon's process environment, which every spawned agent
// subprocess inherits in full (see internal/runenv.Overlay.Apply and
// internal/agent/env.go) unless explicitly scrubbed, and nothing scrubbed
// them. Shelling out to twg means no Bitbucket secret ever needs to enter
// no-mistakes' own environment at all.
//
// twg has no raw Bitbucket Cloud API escape hatch (`twg api` explicitly
// refuses api.bitbucket.org endpoints), so every operation here goes through
// `twg bb ...` porcelain subcommands, mirroring gitea.go's posture toward
// tea. `--workspace`/`--repo` are passed explicitly on every invocation
// (never left to twg's git-remote auto-detection) because the daemon's
// detached bare-gate repo does not have a real Bitbucket git remote, the same
// reason gitea.go always carries --login/--repo.
package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// RepoRef identifies a Bitbucket Cloud repository by workspace and repo slug.
type RepoRef struct {
	Workspace string
	RepoSlug  string
}

// ParseRepoRef extracts a workspace/repo slug pair from a Bitbucket Cloud git
// remote or web URL (bitbucket.org or the scp-like git@bitbucket.org: form).
func ParseRepoRef(raw string) (RepoRef, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSuffix(trimmed, ".git")

	if strings.HasPrefix(trimmed, "git@bitbucket.org:") {
		path := strings.TrimPrefix(trimmed, "git@bitbucket.org:")
		return parseRepoPath(path)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return RepoRef{}, fmt.Errorf("parse bitbucket repo URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "bitbucket.org" && !strings.HasSuffix(host, ".bitbucket.org") {
		return RepoRef{}, fmt.Errorf("unsupported Bitbucket host %q", parsed.Host)
	}
	return parseRepoPath(strings.TrimPrefix(parsed.Path, "/"))
}

func parseRepoPath(path string) (RepoRef, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return RepoRef{}, fmt.Errorf("invalid Bitbucket repository path %q", path)
	}
	return RepoRef{Workspace: parts[0], RepoSlug: parts[1]}, nil
}

// CommitStatus mirrors one entry of Bitbucket's commit-status shape, as
// hydrated by `twg bb pull-requests get --statuses` under the `_statuses` key.
// CreatedOn/UpdatedOn are read so LatestStatuses can establish a verified
// newest-first order instead of trusting the hydration's array order, which
// is not documented as sorted.
type CommitStatus struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	State       string `json:"state"`
	Description string `json:"description"`
	URL         string `json:"url"`
	CreatedOn   string `json:"created_on"`
	UpdatedOn   string `json:"updated_on"`
}

// Host talks to Bitbucket Cloud through the twg CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	repo         RepoRef
	draft        bool // open created PRs as drafts
}

// New builds a Host. cliAvailable reports whether the twg binary is
// resolvable on the caller's PATH. repo identifies the Bitbucket workspace
// and repository every invocation is scoped to. When draft is true, created
// PRs are opened as drafts.
func New(cmd CmdFactory, cliAvailable func() bool, repo RepoRef, draft bool) *Host {
	return &Host{cmd: cmd, cliAvailable: cliAvailable, repo: repo, draft: draft}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderBitbucket }

// Capabilities reports Bitbucket's feature matrix. MergeableState is read from
// the PR's merge checks (`twg bb pull-requests mergeability`), not from a
// merge-conflict probe of its own. ReviewComments and ReviewCommentReplies
// declare the comment thread support in review_comments.go: every unresolved
// human and bot comment is readable and each can be answered.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true, ReviewComments: true, ReviewCommentReplies: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("twg CLI is not installed")
	}
	cmd := h.cmd(ctx, "twg", "whoami")
	if err := cmd.Run(); err != nil {
		return errors.New("twg CLI is not authenticated; run `twg login`")
	}
	return nil
}

func (h *Host) repoArgs() []string {
	return []string{"--workspace", h.repo.Workspace, "--repo", h.repo.RepoSlug}
}

// bitbucketPullRequest is the subset of twg's `pull-requests get`/`query`/
// `create`/`update` JSON this package reads. Field names mirror the
// underlying Bitbucket Cloud API shape verbatim (id, state, links.html.href),
// which twg's own documented agent-field presets confirm it passes through.
type bitbucketPullRequest struct {
	ID    int    `json:"id"`
	State string `json:"state"`
	Draft bool   `json:"draft"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
}

func (pr bitbucketPullRequest) toPR(repo RepoRef) *scm.PR {
	return &scm.PR{
		Number: strconv.Itoa(pr.ID),
		URL:    prURL(repo, pr.ID, pr.Links.HTML.Href),
	}
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	items, err := h.queryPRs(ctx, "OPEN", branch, base)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	// The listing carries the destination, so an existing PR's live base is
	// known without a second read.
	pr := items[0].toPR(h.repo)
	pr.BaseBranch = strings.TrimSpace(items[0].Destination.Branch.Name)
	return pr, nil
}

// GetPRBaseBranch implements scm.PRBaseBranchReader: the PR's destination
// branch, read from `twg bb pull-requests get`. A response without one is an
// error, never an empty base the caller could mistake for "unset".
func (h *Host) GetPRBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return "", errors.New("twg bb pull-requests get: missing pull request number")
	}
	view, _, err := h.getPR(ctx, strings.TrimSpace(pr.Number))
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(view.Destination.Branch.Name)
	if base == "" {
		return "", fmt.Errorf("twg bb pull-requests get: pull request %s has no destination branch", pr.Number)
	}
	return base, nil
}

// SetPRBaseBranch implements scm.PRBaseRetargeter with `twg bb pull-requests
// update --dest`. twg echoes the updated PR; when that echo names a
// destination it must be the requested one, so a silently ignored flag cannot
// read as a successful retarget.
func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	baseBranch = strings.TrimSpace(baseBranch)
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return errors.New("twg bb pull-requests update: missing pull request number")
	}
	if baseBranch == "" {
		return errors.New("twg bb pull-requests update: missing destination branch")
	}
	args := []string{"bb", "pull-requests", "update", "--pull-request", strings.TrimSpace(pr.Number), "--dest", baseBranch}
	args = append(args, h.repoArgs()...)
	out, err := h.runJSON(ctx, args)
	if err != nil {
		return fmt.Errorf("twg bb pull-requests update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var updated bitbucketPullRequest
	if trimmed := bytesTrimToJSON(out); len(trimmed) > 0 && json.Unmarshal(trimmed, &updated) == nil {
		if got := strings.TrimSpace(updated.Destination.Branch.Name); got != "" && got != baseBranch {
			return fmt.Errorf("twg bb pull-requests update: pull request %s still targets %s, not %s", pr.Number, got, baseBranch)
		}
	}
	return nil
}

// FindMergedPRs lists the merged PRs opened from branch, newest first. Only
// the MERGED state is requested, so a DECLINED or SUPERSEDED PR is never
// returned. Every PR carries the target branch and the source commit the forge
// recorded for it, which the caller compares against the commit it is about to
// publish.
func (h *Host) FindMergedPRs(ctx context.Context, branch string) ([]*scm.PR, error) {
	items, err := h.queryPRs(ctx, "MERGED", branch, "")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	merged := make([]*scm.PR, 0, len(items))
	for _, item := range items {
		if name := strings.TrimSpace(item.Source.Branch.Name); name != "" && name != branch {
			continue
		}
		pr := item.toPR(h.repo)
		pr.BaseBranch = strings.TrimSpace(item.Destination.Branch.Name)
		pr.HeadSHA = strings.TrimSpace(item.Source.Commit.Hash)
		merged = append(merged, pr)
	}
	return merged, nil
}

// queryPRs lists PRs from branch in one state. It returns an error for any
// lookup, decoding or validation failure so no caller can read an
// indeterminate answer as "there is no such PR".
func (h *Host) queryPRs(ctx context.Context, state, branch, base string) ([]bitbucketPullRequest, error) {
	args := []string{"bb", "pull-requests", "query", "--source", branch, "--state", state}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--dest", base)
	}
	args = append(args, h.repoArgs()...)
	out, err := h.runJSON(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests query: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		// Nonempty output with no JSON delimiter is not a legitimate "no open
		// PRs" response (an empty list still prints "[]", which has a
		// delimiter) - it must surface as an error rather than be read as
		// absence, which would otherwise cause the PR step to attempt a
		// duplicate create or report a misleading creation failure.
		return nil, fmt.Errorf("twg bb pull-requests query: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if string(trimmed) == "null" {
		// A bare JSON `null` (e.g. a nil slice marshaled at the top level) is
		// not the same thing as a verified empty list "[]": unmarshaling it
		// into []bitbucketPullRequest would silently succeed with a nil
		// slice, indistinguishable from a real "no open PRs" answer, and risk
		// the PR step creating a duplicate. Treat it as an invalid response
		// instead of absence.
		return nil, fmt.Errorf("twg bb pull-requests query: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var items []bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests query: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	for i := range items {
		if items[i].ID <= 0 {
			return nil, fmt.Errorf("twg bb pull-requests query: entry %d missing positive id", i)
		}
		// An entry without its source repository cannot be told apart from a
		// fork PR, so it is an incomplete response, not an absence.
		if strings.TrimSpace(items[i].Source.Repository.FullName) == "" {
			return nil, fmt.Errorf("twg bb pull-requests query: entry %d missing source repository full_name", i)
		}
	}
	// twg's query matches by source branch name alone, so a fork PR that
	// happens to use the same branch name must not be bound. Only a PR whose
	// source repository is this repository counts.
	wantRepo := h.repo.Workspace + "/" + h.repo.RepoSlug
	sameRepo := items[:0]
	for _, item := range items {
		if strings.EqualFold(item.Source.Repository.FullName, wantRepo) {
			sameRepo = append(sameRepo, item)
		}
	}
	return sameRepo, nil
}

// outputSummaryFlag turns off twg's default `--output-summary auto`, which
// replaces any large result with a short YAML envelope pointing at a file.
// That envelope has no JSON delimiter, so a large PR list or statuses payload
// would otherwise fail every parse below with "invalid JSON output".
const outputSummaryFlag = "--output-summary"

// jsonArgs appends the flags every twg call whose stdout is parsed as JSON
// needs: `-o json` plus `--output-summary none` so twg prints the raw
// payload instead of a summary envelope.
func jsonArgs(args []string) []string {
	return append(args, "-o", "json", outputSummaryFlag, "none")
}

// runJSON runs a twg subcommand and returns its combined output. Every
// JSON-parsed twg call goes through it so a new call cannot forget the
// output flags; callers must not pass their own `-o` or `--output-summary`.
func (h *Host) runJSON(ctx context.Context, args []string) ([]byte, error) {
	return h.cmd(ctx, "twg", jsonArgs(args)...).CombinedOutput()
}

// runWithDescription runs a twg bb pull-requests command whose description is
// supplied out-of-band through a temp file referenced by
// `--description-file <path>`, and returns the command's combined output.
// buildArgs receives the file path and returns the full twg argv, placing
// that path where --description-file's value belongs. A temp file (rather
// than passing the body inline as --description) avoids OS argument-length
// limits on large PR bodies and matches twg's own documented convention for
// non-trivial descriptions.
func (h *Host) runWithDescription(ctx context.Context, body string, buildArgs func(descPath string) []string) ([]byte, error) {
	f, err := os.CreateTemp("", "nm-bb-pr-desc-*.md")
	if err != nil {
		return nil, fmt.Errorf("create PR description temp file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return nil, fmt.Errorf("write PR description temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close PR description temp file: %w", err)
	}
	out, err := h.runJSON(ctx, buildArgs(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	out, err := h.runWithDescription(ctx, content.Body, func(descPath string) []string {
		args := []string{"bb", "pull-requests", "create",
			"--title", content.Title,
			"--source", branch,
			"--description-file", descPath,
		}
		if strings.TrimSpace(base) != "" {
			args = append(args, "--dest", base)
		}
		if h.draft {
			args = append(args, "--draft")
		}
		args = append(args, h.repoArgs()...)
		return args
	})
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests create: %w", err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("twg bb pull-requests create: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &pr); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests create: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if pr.ID <= 0 {
		return nil, fmt.Errorf("twg bb pull-requests create: missing positive id: %s", strings.TrimSpace(string(out)))
	}
	return pr.toPR(h.repo), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return nil, errors.New("twg bb pull-requests update: missing pull request number")
	}
	out, err := h.runWithDescription(ctx, content.Body, func(descPath string) []string {
		args := []string{"bb", "pull-requests", "update",
			"--pull-request", pr.Number,
			"--description-file", descPath,
		}
		if strings.TrimSpace(content.Title) != "" {
			args = append(args, "--title", content.Title)
		}
		args = append(args, h.repoArgs()...)
		return args
	})
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests update: %w", err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("twg bb pull-requests update: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var updated bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &updated); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests update: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return updated.toPR(h.repo), nil
}

// DraftLifecycle reports whether PRs are opened as drafts and kept there until
// the run proves them ready (providers.bitbucket.draft_pull_requests).
func (h *Host) DraftLifecycle() bool { return h.draft }

func (h *Host) GetPRDraft(ctx context.Context, pr *scm.PR) (bool, error) {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return false, errors.New("twg bb pull-requests get: missing pull request number")
	}
	view, trimmed, err := h.getPR(ctx, pr.Number)
	if err != nil {
		return false, err
	}
	// A response without the field would read as "ready" and let a draft be
	// published by mistake, so absence is an error rather than false.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return false, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(trimmed)))
	}
	if _, ok := raw["draft"]; !ok {
		return false, errors.New("twg bb pull-requests get: response does not report the draft state")
	}
	return view.Draft, nil
}

// SetPRDraft toggles draft state with `twg bb pull-requests update --draft` or
// `--ready`, without touching the title or description.
func (h *Host) SetPRDraft(ctx context.Context, pr *scm.PR, draft bool) error {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return errors.New("twg bb pull-requests update: missing pull request number")
	}
	flag := "--ready"
	if draft {
		flag = "--draft"
	}
	args := []string{"bb", "pull-requests", "update", "--pull-request", pr.Number, flag}
	args = append(args, h.repoArgs()...)
	if out, err := h.runJSON(ctx, args); err != nil {
		return fmt.Errorf("twg bb pull-requests update %s: %s: %w", flag, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (h *Host) getPR(ctx context.Context, number string, extraArgs ...string) (bitbucketPullRequest, []byte, error) {
	args := append([]string{"bb", "pull-requests", "get", number}, extraArgs...)
	args = append(args, h.repoArgs()...)
	out, err := h.runJSON(ctx, args)
	if err != nil {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &pr); err != nil {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return pr, trimmed, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	view, _, err := h.getPR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return normalizePRState(view.State), nil
}

// pullRequestStatuses reads the PR's hydrated build/CI statuses via
// `twg bb pull-requests get --statuses`, along with the PR's current source
// commit hash so callers that fetch logs by build number can verify a status
// actually belongs to the head they were asked about.
func (h *Host) pullRequestStatuses(ctx context.Context, prNumber string) (bitbucketPullRequest, []CommitStatus, error) {
	_, trimmed, err := h.getPR(ctx, prNumber, "--statuses")
	if err != nil {
		return bitbucketPullRequest{}, nil, err
	}
	var view struct {
		bitbucketPullRequest
		Statuses []CommitStatus `json:"_statuses"`
	}
	if err := json.Unmarshal(trimmed, &view); err != nil {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get --statuses: invalid JSON output: %s", strings.TrimSpace(string(trimmed)))
	}
	return view.bitbucketPullRequest, view.Statuses, nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	_, statuses, err := h.pullRequestStatuses(ctx, pr.Number)
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

// Bitbucket merge checks that decide the PR's mergeable state. Both are checks
// Bitbucket itself evaluates; no-mistakes never counts how far behind a branch
// is.
const (
	mergeCheckGit           = "git_mergeability_check"
	mergeCheckCommitsBehind = "maximum_commits_behind"
)

// mergeCheck is one entry of `twg bb pull-requests mergeability`. The git
// check is identified by its type; the other checks carry their kind under
// `check.kind`. Blocking is a pointer because twg documents that a FAILED
// status alone does not say the check blocks the merge.
type mergeCheck struct {
	Type     string `json:"type"`
	Status   string `json:"status"`
	Blocking *bool  `json:"blocking"`
	Check    struct {
		Kind string `json:"kind"`
	} `json:"check"`
}

func (c mergeCheck) is(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(c.Type), kind) || strings.EqualFold(strings.TrimSpace(c.Check.Kind), kind)
}

// GetMergeableState maps Bitbucket's own merge checks onto the shared
// mergeable state. A failed git mergeability check is a merge conflict. A
// failed, blocking maximum_commits_behind check means Bitbucket refuses the
// merge until the branch is rebased onto its destination, which the CI
// monitor's merge-conflict repair does, so it reads as CONFLICTING too. Any
// check that is PENDING or UNKNOWN, or a response without the git check, is
// PENDING: UNKNOWN means the check could not be evaluated, not that it passed.
// Any other status is a pass, so a status name twg adds later cannot park a
// green PR until the CI timeout.
func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return "", errors.New("twg bb pull-requests mergeability: missing pull request number")
	}
	args := append([]string{"bb", "pull-requests", "mergeability", pr.Number}, h.repoArgs()...)
	out, err := h.runJSON(ctx, args)
	if err != nil {
		return "", fmt.Errorf("twg bb pull-requests mergeability: %s: %w", strings.TrimSpace(string(out)), err)
	}
	checks, err := parseMergeChecks(out)
	if err != nil {
		return "", err
	}
	gitSeen, pending := false, false
	for _, check := range checks {
		isGit, isBehind := check.is(mergeCheckGit), check.is(mergeCheckCommitsBehind)
		if !isGit && !isBehind {
			continue
		}
		if isGit {
			gitSeen = true
		}
		switch strings.ToUpper(strings.TrimSpace(check.Status)) {
		case "FAILED":
			if isGit || check.Blocking == nil || *check.Blocking {
				return scm.MergeableConflict, nil
			}
		case "PENDING", "UNKNOWN", "":
			pending = true
		}
	}
	if pending || !gitSeen {
		return scm.MergeablePending, nil
	}
	return scm.MergeableOK, nil
}

// parseMergeChecks decodes the mergeability listing: a bare array, or an
// object carrying the array under `values`. Anything else is an error so an
// unreadable answer is never read as "mergeable".
func parseMergeChecks(out []byte) ([]mergeCheck, error) {
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("twg bb pull-requests mergeability: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var checks []mergeCheck
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &checks); err != nil {
			return nil, fmt.Errorf("twg bb pull-requests mergeability: invalid JSON output: %s", strings.TrimSpace(string(out)))
		}
		return checks, nil
	}
	var wrapped struct {
		Values *[]mergeCheck `json:"values"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil || wrapped.Values == nil {
		return nil, fmt.Errorf("twg bb pull-requests mergeability: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return *wrapped.Values, nil
}

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

func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _ string, headSHA string, targets []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	view, statuses, err := h.pullRequestStatuses(ctx, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("resolve selected Bitbucket checks: %w", err)
	}
	// The PR's hydrated statuses are not scoped to a commit by the caller;
	// they reflect whatever Bitbucket currently attaches to the PR, which can
	// still be the previous head right after a push. Fetching a pipeline's
	// logs by build number alone would then hand the fixer evidence for a
	// commit that is no longer the one it is fixing. Refuse rather than
	// silently return a stale build's logs when the PR's current source
	// commit does not match the requested head.
	if strings.TrimSpace(headSHA) != "" && !bitbucketCommitMatchesHead(view.Source.Commit.Hash, headSHA) {
		return nil, fmt.Errorf("resolve selected Bitbucket checks: PR %s source commit %q does not match requested head %q", pr.Number, view.Source.Commit.Hash, headSHA)
	}
	targetBuildNumbers := make([]map[string]struct{}, len(targets))
	for i, target := range targets {
		resolved, err := failedPipelineBuildNumberTargets(statuses, []scm.CheckTarget{target})
		if err == nil {
			targetBuildNumbers[i] = resolved
		}
	}
	cache := map[string]scm.FailedCheckLog{}
	results := make([]scm.FailedCheckLog, 0, len(targets))
	for i, target := range targets {
		result := scm.FailedCheckLog{Target: target}
		if len(targetBuildNumbers[i]) == 0 {
			result.Err = fmt.Errorf("selected Bitbucket check %q was not found", target.Identity())
			results = append(results, result)
			continue
		}
		var outputs []string
		var logErrors []error
		for buildNumber := range targetBuildNumbers[i] {
			cached, ok := cache[buildNumber]
			if !ok {
				cached.Output, cached.Err = h.fetchPipelineLog(ctx, buildNumber)
				cache[buildNumber] = cached
			}
			if cached.Output != "" {
				outputs = append(outputs, cached.Output)
			}
			if cached.Err != nil {
				logErrors = append(logErrors, cached.Err)
			}
		}
		result.Output = strings.Join(outputs, "\n\n")
		result.Err = errors.Join(logErrors...)
		results = append(results, result)
	}
	return results, nil
}

// fetchPipelineLog returns the failed-step log text for one pipeline build.
// twg's `pipeline get --logs --failed-steps` already hydrates only the
// failed/error/stopped steps' logs, so unlike the old HTTP client this needs
// no separate step-listing call: the human-readable text output is the log
// payload itself, sidestepping any dependency on undocumented JSON step
// shapes for a value that is opaque text either way.
func (h *Host) fetchPipelineLog(ctx context.Context, buildNumber string) (string, error) {
	args := []string{"bb", "pipeline", "get", "--pipeline", buildNumber, "--logs", "--failed-steps", "--lines", "0", outputSummaryFlag, "none"}
	args = append(args, h.repoArgs()...)
	// Stream into a bounded tail instead of CombinedOutput so a huge failed
	// build log never allocates its whole text in the daemon. The tail is what
	// matters: the failure is at the end of a log.
	tail := &tailBuffer{max: maxPipelineLogBytes}
	cmd := h.cmd(ctx, "twg", args...)
	cmd.Stdout = tail
	cmd.Stderr = tail
	err := cmd.Run()
	out := strings.TrimSpace(tail.String())
	if err != nil {
		return "", fmt.Errorf("twg bb pipeline get --logs: %s: %w", out, err)
	}
	return out, nil
}

// maxPipelineLogBytes bounds the memory held for one pipeline's failed-step
// log, matching the 32 KiB budget the CI fixer applies to log evidence.
const maxPipelineLogBytes = 32 * 1024

// tailBuffer is an io.Writer that retains only the last max bytes written.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

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

// LatestStatuses keeps only the newest status per unique key/name. Newest is
// established from each status's own created_on/updated_on timestamp rather
// than assumed from the hydration's array order, which is not documented as
// newest-first: trusting order alone risks an older pass appearing before a
// newer failure and CI silently reporting the wrong verdict.
func LatestStatuses(statuses []CommitStatus) []CommitStatus {
	ordered := make([]CommitStatus, len(statuses))
	copy(ordered, statuses)
	sort.SliceStable(ordered, func(i, j int) bool {
		return statusTimestamp(ordered[i]).After(statusTimestamp(ordered[j]))
	})
	latest := make([]CommitStatus, 0, len(ordered))
	seen := make(map[string]struct{}, len(ordered))
	for _, status := range ordered {
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

// statusTimestamp returns the best available time for a status (preferring
// created_on, falling back to updated_on), or the zero time when neither
// parses, which sorts as oldest and preserves prior best-effort ordering for
// entries the hydration did not timestamp. created_on is preferred because it
// establishes when a build was created and is stable for a given key: if a
// status is later updated (e.g. transitioning from INPROGRESS to FAILED),
// preferring updated_on would let that edit make an older build outrank a
// newer build's status, picking the wrong CI verdict.
func statusTimestamp(status CommitStatus) time.Time {
	for _, raw := range []string{status.CreatedOn, status.UpdatedOn} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

// bitbucketCommitMatchesHead reports whether prCommitHash (Bitbucket's
// source.commit.hash, which may be the full SHA or an abbreviated prefix of
// it) identifies the same commit as headSHA.
func bitbucketCommitMatchesHead(prCommitHash, headSHA string) bool {
	a := strings.ToLower(strings.TrimSpace(prCommitHash))
	b := strings.ToLower(strings.TrimSpace(headSHA))
	if a == "" || b == "" {
		return false
	}
	if len(a) <= len(b) {
		return strings.HasPrefix(b, a)
	}
	return strings.HasPrefix(a, b)
}

func statusName(status CommitStatus) string {
	name := strings.TrimSpace(status.Name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(status.Key)
}

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

// bytesTrimToJSON skips any banner text before the first '{' or '[', mirroring
// the same defensive scan the Gitea CLI-shelled Host uses: a version-check
// notice or similar stray line before the JSON payload must not break parsing.
func bytesTrimToJSON(out []byte) []byte {
	idx := -1
	for i, b := range out {
		if b == '{' || b == '[' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	return out[idx:]
}

var (
	_ scm.MergedPRFinder     = (*Host)(nil)
	_ scm.PRBaseBranchReader = (*Host)(nil)
	_ scm.PRBaseRetargeter   = (*Host)(nil)
)
