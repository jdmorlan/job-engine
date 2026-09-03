// Package gitsource fetches a job repository from GitHub.
//
// It is deliberately not a git client. D19 ships a `FROM scratch` image and a
// single static binary, so shelling out to `git` is not available and never
// will be, and vendoring a pure-Go git implementation is a large dependency for
// a small need. A tarball over HTTPS is net/http, archive/tar and compress/gzip
// -- all standard library, and it works anywhere the binary works.
//
// It also has a property worth having on purpose: fetching by commit forces the
// pinning question to be answered. A ref is resolved to a commit once, visibly,
// and the fetch is then of something immutable that can be cached forever.
// Without a recorded commit, "what ran?" is unanswerable for a job whose code
// came from a moving branch, and D11 quietly stops being true for every remote
// job.
package gitsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FallbackRef is used only when GitHub will not say what a repository's default
// branch is.
//
// The default is otherwise *asked for* rather than assumed, which matters more
// than it sounds: "main" is a convention, not a rule, and half the repositories
// that exist still call it master. Guessing produces "422 Unprocessable
// Entity" on a repository that is perfectly fine, which is a confusing way to
// learn that your engine has an opinion about branch naming.
//
// Tracking a branch at all is the ergonomic choice and not the safe one, so it
// is paired with two things that are not optional: the resolved commit is
// recorded on every sync, and a change between two syncs is an event carrying
// both revisions. Code changing under a running engine is then a row somebody
// can find rather than something discovered afterwards.
const FallbackRef = "main"

// Repo identifies a GitHub repository.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// ParseRepo accepts the forms somebody would actually type.
func ParseRepo(s string) (Repo, error) {
	// A filesystem path is not a repository, and "/tmp/jobs" splits into two
	// parts exactly like "owner/repo" does. Refused here rather than left to
	// the caller, so the parser and LooksLikeRepo cannot disagree.
	if trimmed := strings.TrimSpace(s); strings.HasPrefix(trimmed, "/") ||
		strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, "~") {
		return Repo{}, fmt.Errorf("%q is a path, not a GitHub repository", s)
	}

	trimmed := strings.TrimSuffix(strings.TrimSpace(s), ".git")
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimPrefix(trimmed, "git@github.com:")
	trimmed = strings.TrimPrefix(trimmed, "github.com/")
	trimmed = strings.Trim(trimmed, "/")

	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Repo{}, fmt.Errorf(
			"%q is not a GitHub repository; write it as owner/repo", s)
	}
	return Repo{Owner: parts[0], Name: parts[1]}, nil
}

// LooksLikeRepo reports whether a string is plausibly owner/repo rather than a
// filesystem path, so `je source add` can tell them apart without a flag.
func LooksLikeRepo(s string) bool {
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
		return false
	}
	if strings.Contains(s, "github.com") || strings.HasPrefix(s, "git@") {
		return true
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// Client talks to the GitHub API.
//
// The zero value works for public repositories. Token authenticates, and is
// read from the engine's secret store by the caller -- this package never sees
// where it came from and never logs it.
type Client struct {
	HTTP  *http.Client
	Token string

	// BaseURL exists for tests. Empty means api.github.com.
	BaseURL string
}

const defaultBaseURL = "https://api.github.com"

// userAgent is required: GitHub rejects requests without one.
const userAgent = "job-engine"

func (c Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// A bounded default: a fetch that hangs at daemon start would hold up
	// everything behind it, and a repository that cannot be reached in two
	// minutes is a problem to report rather than to keep waiting on.
	return &http.Client{Timeout: 2 * time.Minute}
}

// DefaultBranch asks what this repository's default branch is called.
//
// One extra request, made once when a source is registered rather than on every
// sync: the answer is stored as the source's ref, so from then on it is the
// branch you are tracking rather than a question being re-asked.
func (c Client) DefaultBranch(ctx context.Context, repo Repo) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", c.base(), repo.Owner, repo.Name)

	req, err := c.request(ctx, url)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", c.apiError(resp, repo, "")
	}
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("reading %s from GitHub: %w", repo, err)
	}
	if body.DefaultBranch == "" {
		return FallbackRef, nil
	}
	return body.DefaultBranch, nil
}

// ResolveRef turns a branch, tag or commit into the commit it names.
//
// This is the step that makes pinning real. Everything downstream -- the cache
// key, the recorded revision, the from/to in the event -- is this value, so a
// source tracking a branch still records exactly what it ran.
func (c Client) ResolveRef(ctx context.Context, repo Repo, ref string) (string, error) {
	if ref == "" {
		ref = FallbackRef
	}
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", c.base(), repo.Owner, repo.Name, ref)

	req, err := c.request(ctx, url)
	if err != nil {
		return "", err
	}
	// Asks for the commit sha as the whole response body, rather than a
	// commit object we would then have to parse.
	req.Header.Set("Accept", "application/vnd.github.sha")

	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", c.apiError(resp, repo, ref)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(body))
	if len(sha) < 7 {
		return "", fmt.Errorf("GitHub returned %q for %s@%s, which is not a commit", sha, repo, ref)
	}
	return sha, nil
}

// Tarball opens the repository at one commit.
//
// By commit rather than by ref, so what is downloaded is immutable and can be
// cached under its own name forever.
func (c Client) Tarball(ctx context.Context, repo Repo, sha string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/tarball/%s", c.base(), repo.Owner, repo.Name, sha)

	req, err := c.request(ctx, url)
	if err != nil {
		return nil, err
	}

	// GitHub redirects this to codeload with a signed, short-lived URL. Go
	// drops the Authorization header across that host change, which is correct
	// and is also why a private repository still works: the redirect target
	// carries its own credential in the query string.
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching GitHub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, c.apiError(resp, repo, sha)
	}
	return resp.Body, nil
}

func (c Client) request(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// apiError turns a status code into something a person can act on.
//
// The distinction that matters most is 404: for a private repository GitHub
// returns "not found" rather than "forbidden", so an unauthenticated request
// and a genuinely missing repository are indistinguishable from the status
// alone. Saying both possibilities is the honest answer.
func (c Client) apiError(resp *http.Response, repo Repo, ref string) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		if c.Token == "" {
			return fmt.Errorf(
				"GitHub has no %s@%s -- or it is private, in which case it needs a token: "+
					"je secret set GITHUB_TOKEN, then --token GITHUB_TOKEN", repo, ref)
		}
		return fmt.Errorf(
			"GitHub has no %s@%s, or the token cannot see it (it needs repo read access)",
			repo, ref)
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub rejected the token for %s: it is invalid or expired", repo)
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			reset := resp.Header.Get("X-RateLimit-Reset")
			return fmt.Errorf(
				"GitHub rate limit reached (resets at unix %s); authenticating raises it a long way",
				reset)
		}
		return fmt.Errorf("GitHub refused access to %s", repo)
	case http.StatusUnprocessableEntity:
		// What GitHub says when the repository exists and the ref does not.
		return fmt.Errorf("%s has no branch, tag or commit called %q", repo, ref)
	default:
		return fmt.Errorf("GitHub returned %s for %s@%s", resp.Status, repo, ref)
	}
}
