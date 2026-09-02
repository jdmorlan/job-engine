// Package selfupdate finds, verifies and installs new versions of the binary.
//
// D19's argument about deployment applies to the binary itself: a tool that is
// awkward to install is a tool nobody installs, and the quality of the engine
// is then irrelevant. Building this now rather than later is also the only way
// to know it works -- an upgrade path exercised for the first time on a real
// upgrade is an upgrade path that fails on a real upgrade.
//
// The whole mechanism is deliberately small and dependency-free: a JSON request
// to the releases API, a tarball over HTTPS, a SHA-256 check, and a rename.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// DefaultRepo is where releases come from. Overridable so a fork, or a test,
// can point somewhere else.
const DefaultRepo = "jdmorlan/job-engine"

// DefaultAPI is GitHub's REST endpoint. A field rather than a constant in the
// Client below so tests can serve their own.
const DefaultAPI = "https://api.github.com"

// Release is one published version.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Assets      []Asset   `json:"assets"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Client talks to the release host.
type Client struct {
	Repo string
	API  string
	HTTP *http.Client
}

// NewClient returns a client with sensible defaults.
//
// Both the repository and the API host are overridable from the environment.
// That is not only for tests: a fork wants its own releases, and a GitHub
// Enterprise install is not api.github.com. Hard-coding the host would make
// this the one part of the tool that only works for its author.
func NewClient() *Client {
	api := DefaultAPI
	if override := os.Getenv("JE_RELEASE_API"); override != "" {
		api = strings.TrimSuffix(override, "/")
	}
	repo := DefaultRepo
	if override := os.Getenv("JE_RELEASE_REPO"); override != "" {
		repo = override
	}
	return &Client{
		Repo: repo,
		API:  api,
		// A real timeout, unlike the API client: checking for updates must
		// never be the reason a command hangs.
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// Latest returns the most recent non-draft, non-prerelease version.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.API, c.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	// Asking for the versioned media type means a future API change cannot
	// silently alter the shape we parse.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("checking for releases: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Either the repo is private or nothing has been released yet. Both
		// are ordinary situations for a young project, and neither deserves a
		// stack trace.
		return Release{}, fmt.Errorf("no releases published for %s yet", c.Repo)
	case http.StatusForbidden:
		return Release{}, fmt.Errorf(
			"GitHub rate-limited this check; it resets within the hour")
	default:
		return Release{}, fmt.Errorf("checking for releases: %s", resp.Status)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Release{}, fmt.Errorf("reading release information: %w", err)
	}
	return release, nil
}

// AssetName is what a release calls the archive for one platform.
//
// Kept as a function rather than a format string in three places, because the
// release workflow, the install script and this code all have to agree, and
// the way they stop agreeing is by drifting one at a time.
func AssetName(version, goos, goarch string) string {
	return fmt.Sprintf("je_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), goos, goarch)
}

// ChecksumsName is the file listing SHA-256 sums for every asset.
const ChecksumsName = "checksums.txt"

// AssetFor picks the archive matching the current platform.
func (r Release) AssetFor(goos, goarch string) (Asset, error) {
	want := AssetName(r.TagName, goos, goarch)
	for _, a := range r.Assets {
		if a.Name == want {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s has no build for %s/%s", r.TagName, goos, goarch)
}

// Checksums finds the checksum manifest.
func (r Release) Checksums() (Asset, error) {
	for _, a := range r.Assets {
		if a.Name == ChecksumsName {
			return a, nil
		}
	}
	// Refusing rather than installing unverified is the right default: this
	// downloads an executable and runs it as you.
	return Asset{}, fmt.Errorf("release %s publishes no %s, so the download cannot be verified",
		r.TagName, ChecksumsName)
}

// Platform reports the current build target, matching the release naming.
func Platform() (string, string) { return runtime.GOOS, runtime.GOARCH }
