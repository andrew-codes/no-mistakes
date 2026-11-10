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

// Fork releases are tagged v<upstream version>-andrew-codes.N: a semver
// prerelease of the upstream version whose first identifier contains a hyphen.
func TestForkTagsOrderAsSemverAndAreNotDevVersions(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		wantCmp int
	}{
		{"next revision", "v1.111.0-andrew-codes.1", "v1.111.0-andrew-codes.2", -1},
		{"revisions compare numerically", "v1.111.0-andrew-codes.9", "v1.111.0-andrew-codes.10", -1},
		{"later upstream base", "v1.111.0-andrew-codes.2", "v1.112.0-andrew-codes.1", -1},
		{"same tag", "v1.111.0-andrew-codes.1", "v1.111.0-andrew-codes.1", 0},
		{"prerelease of its own upstream base", "v1.111.0-andrew-codes.1", "v1.111.0", -1},
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
	for _, tag := range []string{"v1.111.0-andrew-codes.1", "v1.112.0-andrew-codes.1"} {
		if isDevVersion(tag) {
			t.Fatalf("isDevVersion(%q) = true, want false so update checks stay on", tag)
		}
	}
}

// forkRelease is a fork release as GitHub's REST API returns it: tagged
// v<base>-andrew-codes.N but published as a normal, non-prerelease release.
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
		forkRelease("v1.112.0-andrew-codes.1"),
		forkRelease("v1.111.0-andrew-codes.2"),
		forkRelease("v1.111.0-andrew-codes.1"),
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

	tests := []struct {
		name    string
		current string
		beta    bool
		want    string // empty: already up to date
	}{
		{"stable next revision", "v1.111.0-andrew-codes.1", false, "v1.112.0-andrew-codes.1"},
		{"beta next revision", "v1.111.0-andrew-codes.1", true, "v1.112.0-andrew-codes.1"},
		{"stable later upstream base", "v1.111.0-andrew-codes.2", false, "v1.112.0-andrew-codes.1"},
		{"beta later upstream base", "v1.111.0-andrew-codes.2", true, "v1.112.0-andrew-codes.1"},
		{"stable current", "v1.112.0-andrew-codes.1", false, ""},
		{"beta current", "v1.112.0-andrew-codes.1", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &updater{
				appName:            "no-mistakes",
				currentVersion:     tt.current,
				platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
				manifestURL:        server.URL + "/channels.json",
				httpClient:         server.Client(),
				includePrereleases: tt.beta,
			}
			plan, err := u.checkLatest(context.Background())
			if err != nil {
				t.Fatalf("checkLatest: %v", err)
			}
			if plan.LatestVersion != "v1.112.0-andrew-codes.1" {
				t.Fatalf("LatestVersion = %q", plan.LatestVersion)
			}
			if got := plan.UpdateAvailable; got != (tt.want != "") {
				t.Fatalf("UpdateAvailable = %v, want %v", got, tt.want != "")
			}
			if tt.want != "" && plan.ArchiveName != "no-mistakes-"+tt.want+"-darwin-arm64.tar.gz" {
				t.Fatalf("ArchiveName = %q", plan.ArchiveName)
			}
		})
	}
}

func TestForkBetaChannelPicksHighestRevisionRegardlessOfListOrder(t *testing.T) {
	all := []releaseResponse{
		forkRelease("v1.111.0-andrew-codes.1"),
		forkRelease("v1.111.0-andrew-codes.10"),
		forkRelease("v1.111.0-andrew-codes.2"),
	}
	allJSON, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeChannelsManifest(nil, allJSON)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseChannelsManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := m.channel(true)
	if err != nil {
		t.Fatal(err)
	}
	if beta.TagName != "v1.111.0-andrew-codes.10" {
		t.Fatalf("beta = %q, want v1.111.0-andrew-codes.10", beta.TagName)
	}
}
