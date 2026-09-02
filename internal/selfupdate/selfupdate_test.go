package selfupdate_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/selfupdate"
)

// tarGz builds an archive containing one file, the way the release workflow
// does.
func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeRelease serves a release, its archive and its checksums, standing in for
// GitHub so the tests need no network.
func fakeRelease(t *testing.T, tag string, archive []byte, checksums string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	assetName := selfupdate.AssetName(tag, "linux", "amd64")

	mux.HandleFunc("/repos/acme/je/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(selfupdate.Release{
			TagName: tag,
			Assets: []selfupdate.Asset{
				{Name: assetName, URL: srv.URL + "/dl/" + assetName, Size: int64(len(archive))},
				{Name: selfupdate.ChecksumsName, URL: srv.URL + "/dl/" + selfupdate.ChecksumsName},
			},
		})
	})
	mux.HandleFunc("/dl/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/dl/"+selfupdate.ChecksumsName, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksums)
	})
	return srv
}

func TestLatestAndAssetSelection(t *testing.T) {
	archive := tarGz(t, "je", []byte("#!/bin/sh\necho v2\n"))
	name := selfupdate.AssetName("v0.2.0", "linux", "amd64")
	srv := fakeRelease(t, "v0.2.0", archive, sha256Hex(archive)+"  "+name+"\n")

	c := selfupdate.NewClient()
	c.Repo, c.API = "acme/je", srv.URL

	release, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if release.TagName != "v0.2.0" {
		t.Errorf("tag = %q", release.TagName)
	}

	asset, err := release.AssetFor("linux", "amd64")
	if err != nil {
		t.Fatalf("AssetFor: %v", err)
	}
	if asset.Name != name {
		t.Errorf("asset = %q, want %q", asset.Name, name)
	}

	// A platform with no build must say so rather than returning something
	// almost right.
	if _, err := release.AssetFor("windows", "386"); err == nil {
		t.Error("AssetFor returned a build for a platform that was not published")
	}
}

// TestChecksumsAreRequired covers the default that matters: this downloads an
// executable and puts it on your PATH, so an unverifiable release is refused
// rather than installed hopefully.
func TestChecksumsAreRequired(t *testing.T) {
	release := selfupdate.Release{
		TagName: "v0.1.0",
		Assets:  []selfupdate.Asset{{Name: selfupdate.AssetName("v0.1.0", "linux", "amd64")}},
	}
	if _, err := release.Checksums(); err == nil {
		t.Fatal("a release without checksums was accepted")
	}
}

func TestParseChecksums(t *testing.T) {
	// Both sha256sum spellings: text mode and binary mode's leading asterisk.
	body := "aaaa  je_0.1.0_linux_amd64.tar.gz\nbbbb *je_0.1.0_darwin_arm64.tar.gz\n"
	sums, err := selfupdate.ParseChecksums(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if sums["je_0.1.0_linux_amd64.tar.gz"] != "aaaa" {
		t.Errorf("text-mode line not parsed: %v", sums)
	}
	if sums["je_0.1.0_darwin_arm64.tar.gz"] != "bbbb" {
		t.Errorf("binary-mode line not parsed: %v", sums)
	}

	if _, err := selfupdate.ParseChecksums(strings.NewReader("")); err == nil {
		t.Error("an empty checksum file was accepted")
	}
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	body := []byte("binary contents")
	archivePath := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archivePath, tarGz(t, "je", body), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := selfupdate.ExtractBinary(archivePath, "je", dir)
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("extracted %q, want %q", got, body)
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("extracted binary is not executable")
	}
}

// TestExtractRejectsTraversal covers the oldest trick there is. This is the
// only place in the program that unpacks an archive fetched from the network.
func TestExtractRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "evil.tar.gz")
	if err := os.WriteFile(archivePath,
		tarGz(t, "../../../../tmp/pwned", []byte("nope")), 0o644); err != nil {
		t.Fatal(err)
	}

	// The traversing entry's base name is "pwned", not "je", so it is not
	// extracted at all -- and crucially nothing is ever written using a path
	// that came out of the archive.
	if _, err := selfupdate.ExtractBinary(archivePath, "je", dir); err == nil {
		t.Fatal("a traversing archive was accepted")
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("extraction escaped the destination directory")
	}
}

func TestReplaceIsAtomicAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "je")
	if err := os.WriteFile(target, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, ".new")
	if err := os.WriteFile(replacement, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := selfupdate.Replace(target, replacement); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Errorf("target = %q, want new", body)
	}
	// A deliberately tightened install must not be widened by an upgrade.
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, want the existing 0750 to be preserved", info.Mode().Perm())
	}
}

func TestManagedElsewhereIsDetected(t *testing.T) {
	tests := map[string]bool{
		"/opt/homebrew/Cellar/je/0.1.0/bin/je": true,
		"/nix/store/abc-je/bin/je":             true,
		"/Users/you/.local/bin/je":             false,
		"/usr/local/bin/je":                    false,
	}
	for path, managed := range tests {
		if got := selfupdate.ManagedElsewhere(path) != ""; got != managed {
			t.Errorf("ManagedElsewhere(%q) managed=%v, want %v", path, got, managed)
		}
	}
}
