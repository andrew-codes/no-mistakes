package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const bbHeadPRURL = "https://bitbucket.org/test/repo/pull-requests/42"

// bitbucketHeadContext is a Bitbucket run whose Push step recorded the real
// local head, with the fake twg's pull request reporting whatever env asks.
func bitbucketHeadContext(t *testing.T, route string, env map[string]string) (*pipeline.StepContext, *fakeBitbucketAPI) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var api *fakeBitbucketAPI
	if route == "update" {
		api = newFakeBitbucketAPI(t, 42, bbHeadPRURL)
	} else {
		api = newFakeBitbucketAPI(t, 0, "")
	}
	for k, v := range env {
		api.env[k] = v
	}
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = api.Env()
	completeCleanReview(t, sctx)
	return sctx, api
}

// The head is proven against twg's own read of the PR, so the body carries no
// "head proof unavailable" notice and the PR is recorded for the run.
func TestPRStep_BitbucketProvesThePushedHead(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"create", "update"} {
		for name, env := range map[string]map[string]string{
			"twg's 12 character abbreviation": nil,
			"the full object ID":              {"FAKE_CLI_BB_FULL_HASH": "1"},
		} {
			t.Run(route+"/"+name, func(t *testing.T) {
				t.Parallel()
				sctx, api := bitbucketHeadContext(t, route, env)
				assertProven := captureProvenPRHead(t, sctx)
				out, err := (&PRStep{}).Execute(sctx)
				if err != nil || out == nil || out.PRURL == "" {
					t.Fatalf("matching head failed: %+v %v", out, err)
				}
				assertProven(api.lastDescription(t))
				if api.logCount("pull-requests get") == 0 {
					t.Fatal("the head was not read back from the PR")
				}
				run, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil || run.PRURL == nil || *run.PRURL != out.PRURL {
					t.Fatalf("PR URL was not recorded for the run: %+v %v", run, err)
				}
			})
		}
	}
}

// Anything but a positive proof refuses publication and leaves the push's
// custody evidence alone, as for GitHub.
func TestPRStep_BitbucketRefusesAnUnprovenHead(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"create", "update"} {
		for _, fault := range []struct {
			name string
			env  map[string]string
			want string
		}{
			{"moved head", map[string]string{"FAKE_CLI_BB_SOURCE_COMMIT": "fedcba987654"}, "does not equal successfully pushed head"},
			{"force-moved head", map[string]string{"FAKE_CLI_BB_SOURCE_COMMIT": "fedcba9876543210fedcba9876543210fedcba98"}, "does not equal successfully pushed head"},
			{"missing source commit", map[string]string{"FAKE_CLI_BB_OMIT_SOURCE_COMMIT": "1"}, "live head is missing"},
			{"abbreviation too short to prove", map[string]string{"FAKE_CLI_BB_SOURCE_COMMIT": "abc"}, "live head is missing or too short"},
			{"other source branch", map[string]string{"FAKE_CLI_BB_SOURCE_BRANCH": "other"}, "source branch does not match"},
			{"twg outage when the head is read", map[string]string{"FAKE_CLI_BB_GET_FAIL_AFTER_PUBLISH": "1"}, "the API is down"},
		} {
			t.Run(route+"/"+fault.name, func(t *testing.T) {
				t.Parallel()
				sctx, api := bitbucketHeadContext(t, route, fault.env)
				out, err := (&PRStep{}).Execute(sctx)
				if err == nil || !strings.Contains(err.Error(), "verify published PR") || !strings.Contains(err.Error(), fault.want) {
					t.Fatalf("unproven head published: out=%+v err=%v, want a refusal containing %q", out, err, fault.want)
				}
				run, getErr := sctx.DB.GetRun(sctx.Run.ID)
				if getErr != nil || run.PRURL != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != sctx.Run.HeadSHA {
					t.Fatalf("refusal changed publication evidence: %+v %v", run, getErr)
				}
				_ = api
			})
		}
	}
}

// A head that has not reached the PR yet is waited for, as for GitHub: the
// forge reports the previous head's abbreviation, which must be matched to the
// previous head by prefix for the retry to apply.
func TestSameCommitMatchesAnAbbreviationToTheFullObjectID(t *testing.T) {
	t.Parallel()
	const full = "0123456789abcdef0123456789abcdef01234567"
	for name, tt := range map[string]struct {
		a, b string
		want bool
	}{
		"equal":                      {full, full, true},
		"case only":                  {full, strings.ToUpper(full), true},
		"12 character prefix":        {full[:12], full, true},
		"prefix on the other side":   {full, full[:12], true},
		"too short to identify":      {full[:11], full, false},
		"different commit":           {"fedcba987654", full, false},
		"empty":                      {"", full, false},
		"both empty":                 {"", "", false},
		"abbreviation of a neighbor": {full[:11] + "0", full, false},
	} {
		if got := sameCommit(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: sameCommit(%q, %q) = %v, want %v", name, tt.a, tt.b, got, tt.want)
		}
	}
}

// A run whose push was skipped has no head to prove, so Bitbucket now refuses
// publication the way GitHub does instead of publishing an unbound PR.
func TestPRStep_BitbucketRefusesPublicationWithoutSuccessfulPushEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketAPI(t, 0, "")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = api.Env()
	completeCleanReview(t, sctx)
	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "complete successful push binding is unavailable") {
		t.Fatalf("Execute() = %v, want a refusal for missing push evidence", err)
	}
	if api.logCount("pull-requests create") != 0 {
		t.Fatal("a PR was created for a run with no successful push")
	}
}
