// Package bitbucket implements scm.Host for Bitbucket Cloud by shelling out
// to the twg CLI (`twg bitbucket ...` / its `twg bb ...` alias), mirroring
// the CLI-adapter pattern used by internal/scm/github, internal/scm/gitlab,
// and internal/scm/azuredevops. twg owns Bitbucket credentials and its own
// auth/login lifecycle end to end; this package never handles a Bitbucket
// App Password, email, or API token itself.
package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

type RepoRef struct {
	Workspace string
	RepoSlug  string
}

type PullRequest struct {
	ID               int
	URL              string
	State            string
	SourceCommitHash string
}

type CommitStatus struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	State       string `json:"state"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env,
// matching the pattern used by internal/scm/github, internal/scm/gitlab, and
// internal/scm/azuredevops.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Client talks to Bitbucket Cloud through the twg CLI.
type Client struct {
	cmd CmdFactory
}

// NewClient builds a Client that shells out to twg via cmd.
func NewClient(cmd CmdFactory) *Client {
	return &Client{cmd: cmd}
}

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

// repoArgs returns the --workspace/--repo flags shared by every twg
// bitbucket invocation for repo.
func repoArgs(repo RepoRef) []string {
	return []string{"--workspace", repo.Workspace, "--repo", repo.RepoSlug}
}

// twgEnvelope is the standard `--output json` wrapper twg emits on every
// command: {"apiVersion":"v2","command":"...","data":<payload>}.
type twgEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// run executes `twg <args...> --output json` and returns the unwrapped
// "data" payload. twg's `-o/--output json` flag (undocumented alias
// `--output-summary`/`--agent-fields` are opt-in only and left unset here) is
// documented to emit "pure machine-readable JSON on stdout", so stdout is
// parsed directly with no banner-skipping needed.
func (c *Client) run(ctx context.Context, args ...string) (json.RawMessage, error) {
	args = append(append([]string{}, args...), "--output", "json")
	cmd := c.cmd(ctx, "twg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("twg %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	var envelope twgEnvelope
	if err := json.Unmarshal(out, &envelope); err != nil {
		return nil, fmt.Errorf("twg %s: decode response: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return envelope.Data, nil
}

// decodeList extracts the item array from a twg list payload, which is
// either a bare JSON array or an object carrying it under "values" (the
// Bitbucket Cloud REST pagination envelope) or "items".
func decodeList(data json.RawMessage) ([]json.RawMessage, error) {
	var asArray []json.RawMessage
	if err := json.Unmarshal(data, &asArray); err == nil {
		return asArray, nil
	}
	var asObject struct {
		Values []json.RawMessage `json:"values"`
		Items  []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &asObject); err != nil {
		return nil, fmt.Errorf("decode twg list payload: %w", err)
	}
	if len(asObject.Values) > 0 {
		return asObject.Values, nil
	}
	return asObject.Items, nil
}

type bitbucketPullRequest struct {
	ID     int    `json:"id"`
	State  string `json:"state"`
	Source struct {
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"source"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (pr bitbucketPullRequest) toPullRequest() *PullRequest {
	return &PullRequest{
		ID:               pr.ID,
		URL:              strings.TrimSpace(pr.Links.HTML.Href),
		State:            strings.TrimSpace(pr.State),
		SourceCommitHash: strings.TrimSpace(pr.Source.Commit.Hash),
	}
}

// FindOpenPRBySourceBranch looks up the open PR from branch to destBranch, if any.
func (c *Client) FindOpenPRBySourceBranch(ctx context.Context, repo RepoRef, branch, destBranch string) (*PullRequest, error) {
	args := append([]string{"bitbucket", "pull-requests", "query", "--scope", "repo", "--source", branch, "--state", "OPEN"}, repoArgs(repo)...)
	if strings.TrimSpace(destBranch) != "" {
		args = append(args, "--dest", destBranch)
	}
	data, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	items, err := decodeList(data)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(items[0], &pr); err != nil {
		return nil, fmt.Errorf("decode Bitbucket pull request: %w", err)
	}
	return pr.toPullRequest(), nil
}

// CreatePR creates a new pull request from sourceBranch to destBranch.
func (c *Client) CreatePR(ctx context.Context, repo RepoRef, sourceBranch, destBranch, title, body string) (*PullRequest, error) {
	args := append([]string{"bitbucket", "pull-requests", "create",
		"--title", title,
		"--source", sourceBranch,
		"--dest", destBranch,
		"--description", body,
	}, repoArgs(repo)...)
	data, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("decode Bitbucket pull request: %w", err)
	}
	return pr.toPullRequest(), nil
}

// UpdatePR updates an existing pull request's title and description.
func (c *Client) UpdatePR(ctx context.Context, repo RepoRef, prID int, title, body string) (*PullRequest, error) {
	args := append([]string{"bitbucket", "pull-requests", "update",
		"--pull-request", strconv.Itoa(prID),
		"--title", title,
		"--description", body,
	}, repoArgs(repo)...)
	data, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("decode Bitbucket pull request: %w", err)
	}
	return pr.toPullRequest(), nil
}

// GetPR fetches a pull request's current state.
func (c *Client) GetPR(ctx context.Context, repo RepoRef, prID int) (*PullRequest, error) {
	args := append([]string{"bitbucket", "pull-requests", "get", strconv.Itoa(prID)}, repoArgs(repo)...)
	data, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("decode Bitbucket pull request: %w", err)
	}
	return pr.toPullRequest(), nil
}

// ListPRStatuses lists the build/CI statuses attached to a pull request.
func (c *Client) ListPRStatuses(ctx context.Context, repo RepoRef, prID int) ([]CommitStatus, error) {
	args := append([]string{"bitbucket", "pull-requests", "get", strconv.Itoa(prID), "--statuses"}, repoArgs(repo)...)
	data, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Statuses []CommitStatus `json:"_statuses"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode Bitbucket pull request statuses: %w", err)
	}
	return payload.Statuses, nil
}

// GetFailedStepLog fetches the log of the first FAILED/ERROR/STOPPED step in
// the given pipeline (identified by UUID, build number, or result URL).
// Returns "" when the pipeline has no failed step or produced no log.
func (c *Client) GetFailedStepLog(ctx context.Context, repo RepoRef, pipelineRef string) (string, error) {
	args := append([]string{"bitbucket", "pipeline", "get",
		"--pipeline", pipelineRef,
		"--logs", "--failed-steps",
	}, repoArgs(repo)...)
	data, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	var payload struct {
		Steps []struct {
			State struct {
				Result struct {
					Name string `json:"name"`
				} `json:"result"`
			} `json:"state"`
			Log string `json:"log"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode Bitbucket pipeline: %w", err)
	}
	for _, step := range payload.Steps {
		result := strings.ToUpper(strings.TrimSpace(step.State.Result.Name))
		if result != "FAILED" && result != "ERROR" && result != "STOPPED" {
			continue
		}
		if log := strings.TrimSpace(step.Log); log != "" {
			return log, nil
		}
	}
	return "", nil
}
