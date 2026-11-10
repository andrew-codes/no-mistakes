package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

func setUpdateRepo(t *testing.T, repo string) {
	t.Helper()
	prev := buildinfo.UpdateRepo
	buildinfo.UpdateRepo = repo
	t.Cleanup(func() { buildinfo.UpdateRepo = prev })
}

func TestUpdateRepoDefaultsToUpstream(t *testing.T) {
	setUpdateRepo(t, "")
	if got := updateRepo(); got != "kunchenguid/no-mistakes" {
		t.Fatalf("updateRepo() = %q, want the upstream repository", got)
	}
}

func TestUpdateRepoUsesBuildTimeOverride(t *testing.T) {
	setUpdateRepo(t, "andrew-codes/no-mistakes")
	if got := updateRepo(); got != "andrew-codes/no-mistakes" {
		t.Fatalf("updateRepo() = %q, want the override", got)
	}
}

func TestDefaultUpdaterManifestURLFollowsUpdateRepo(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	tests := []struct {
		name string
		repo string
		want string
	}{
		{"default", "", "https://github.com/kunchenguid/no-mistakes/releases/download/channels/channels.json"},
		{"fork", "andrew-codes/no-mistakes", "https://github.com/andrew-codes/no-mistakes/releases/download/channels/channels.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setUpdateRepo(t, tt.repo)
			u, err := defaultUpdater(nil, nil)
			if err != nil {
				t.Fatalf("defaultUpdater: %v", err)
			}
			if u.manifestURL != tt.want {
				t.Fatalf("manifestURL = %q, want %q", u.manifestURL, tt.want)
			}
		})
	}
}

// The fork releases on its own plain semver line (v1.0.0, v1.1.0, ...), so the
// updater needs no special handling: a newer fork release is offered to an
// older fork build, ordered numerically rather than lexically.
func TestForkSemverOrdering(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		wantCmp int
	}{
		{"patch", "v1.0.0", "v1.0.1", -1},
		{"minor", "v1.0.1", "v1.1.0", -1},
		{"major", "v1.9.9", "v2.0.0", -1},
		{"numeric not lexical", "v1.9.0", "v1.10.0", -1},
		{"same", "v1.2.3", "v1.2.3", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compareVersions(tt.a, tt.b)
			if err != nil {
				t.Fatalf("compareVersions(%q, %q) error = %v", tt.a, tt.b, err)
			}
			if got != tt.wantCmp {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.wantCmp)
			}
			if rev, _ := compareVersions(tt.b, tt.a); rev != -tt.wantCmp {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.b, tt.a, rev, -tt.wantCmp)
			}
		})
	}
	if isDevVersion("v1.0.0") {
		t.Fatal("a fork release version must keep update checks on")
	}
}

// forkRelease is a fork release as GitHub's REST API returns it: a plain
// semver tag on a normal, non-prerelease release.
func forkRelease(tag string) releaseResponse {
	return releaseResponse{
		TagName: tag,
		Assets: []releaseAsset{
			{Name: "no-mistakes-" + tag + "-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.com/" + tag + ".tgz"},
			{Name: checksumsAssetName, BrowserDownloadURL: "https://example.com/" + tag + "-sums"},
		},
	}
}

// Drives the real publisher encoder (EncodeChannelsManifest) and the real
// updater against the resulting manifest, on both channels.
func TestForkReleasesAreOfferedOnStableAndBetaChannels(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	// GitHub's list endpoint is newest first; "latest" is the newest
	// non-prerelease release.
	all := []releaseResponse{
		forkRelease("v1.10.0"),
		forkRelease("v1.9.0"),
		forkRelease("v1.0.0"),
	}
	allJSON, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	latestJSON, err := json.Marshal(all[0])
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := EncodeChannelsManifest(latestJSON, allJSON)
	if err != nil {
		t.Fatalf("EncodeChannelsManifest: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(manifest); err != nil {
			t.Errorf("write manifest: %v", err)
		}
	}))
	defer server.Close()

	for _, beta := range []bool{false, true} {
		channel := "stable"
		if beta {
			channel = "beta"
		}
		for _, current := range []string{"v1.0.0", "v1.9.0"} {
			t.Run(channel+" offers v1.10.0 to "+current, func(t *testing.T) {
				plan := checkForkUpdate(t, server.URL, current, beta)
				if !plan.UpdateAvailable || plan.LatestVersion != "v1.10.0" {
					t.Fatalf("plan = %+v, want an update to v1.10.0", plan)
				}
				if want := "no-mistakes-v1.10.0-darwin-arm64.tar.gz"; plan.ArchiveName != want {
					t.Fatalf("ArchiveName = %q, want %q", plan.ArchiveName, want)
				}
			})
		}
		t.Run(channel+" is up to date at v1.10.0", func(t *testing.T) {
			plan := checkForkUpdate(t, server.URL, "v1.10.0", beta)
			if plan.UpdateAvailable {
				t.Fatalf("plan = %+v, want no update", plan)
			}
		})
	}
}

func checkForkUpdate(t *testing.T, serverURL, current string, beta bool) *releasePlan {
	t.Helper()
	u := &updater{
		appName:            "no-mistakes",
		currentVersion:     current,
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		manifestURL:        serverURL + "/channels.json",
		httpClient:         http.DefaultClient,
		includePrereleases: beta,
	}
	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest: %v", err)
	}
	return plan
}

// The fork's stable channel is GitHub's latest non-prerelease release, so a
// release published as a prerelease only reaches users on the beta channel.
func TestForkPrereleaseOnlyReachesBetaChannel(t *testing.T) {
	stable := forkRelease("v1.0.0")
	pre := forkRelease("v1.1.0")
	pre.Prerelease = true
	allJSON, err := json.Marshal([]releaseResponse{pre, stable})
	if err != nil {
		t.Fatal(err)
	}
	latestJSON, err := json.Marshal(stable)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeChannelsManifest(latestJSON, allJSON)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseChannelsManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	for beta, want := range map[bool]string{false: "v1.0.0", true: "v1.1.0"} {
		got, err := m.channel(beta)
		if err != nil {
			t.Fatal(err)
		}
		if got.TagName != want {
			t.Fatalf("channel(beta=%v) = %q, want %q", beta, got.TagName, want)
		}
	}
}
