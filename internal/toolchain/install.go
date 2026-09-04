package toolchain

import (
	"fmt"
	"runtime"
	"strings"
)

// How a toolchain is obtained, as data (D28).
//
// The rule this encodes: nothing is installed that cannot be verified. `je
// upgrade` already refuses to replace its own binary on a checksum mismatch,
// and downloading a compiler to run somebody's code deserves at least that.
// A tool whose publisher ships no checksum has no recipe here, and the command
// says so rather than fetching it anyway.

// Recipe is where a tool comes from and how its download is checked.
type Recipe struct {
	// Version is pinned rather than resolved. A worker that installed a
	// toolchain last week and one that installs it today should be running the
	// same thing, and "latest" makes that untrue in a way nobody sees until two
	// machines disagree about a build.
	Version string

	// Archive is the download, with {version}, {os}, {arch} substituted.
	Archive string

	// Checksums is where the SHA-256 comes from.
	//
	// Sibling means <archive>.sha256 holding "<sha>  <name>". List means a file
	// of that form covering every asset, which is the format `je` publishes for
	// itself -- so selfupdate.ParseChecksums reads it unchanged.
	Checksums string
	List      bool

	// Strip is how many leading path components the archive wraps its contents
	// in, the way GitHub wraps a tarball.
	Strip int

	// Binary is the path inside the extracted archive, relative, that should
	// end up on PATH.
	Binary string

	// Then is run after extraction, from the install directory, to obtain
	// anything the archive does not itself contain.
	Then [][]string

	// Arch and OS translate Go's names into the publisher's.
	OS   map[string]string
	Arch map[string]string
}

// recipes are the tools this engine will install.
//
// pnpm is deliberately absent as a direct download: its GitHub releases ship no
// checksum file, so there is nothing to verify an archive against. It is
// obtained through Node instead, whose releases do publish SHASUMS256.txt, and
// npm verifies the integrity of what it installs -- so every link in that chain
// is checked, which a bare download of pnpm would not be.
var recipes = map[string]Recipe{
	"uv": {
		Version:   "0.5.11",
		Archive:   "https://github.com/astral-sh/uv/releases/download/{version}/uv-{arch}-{os}.tar.gz",
		Checksums: "https://github.com/astral-sh/uv/releases/download/{version}/uv-{arch}-{os}.tar.gz.sha256",
		Strip:     1,
		Binary:    "uv",
		OS:        map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-gnu"},
		Arch:      map[string]string{"arm64": "aarch64", "amd64": "x86_64"},
	},
	"pnpm": {
		Version:   "v22.14.0",
		Archive:   "https://nodejs.org/dist/{version}/node-{version}-{os}-{arch}.tar.gz",
		Checksums: "https://nodejs.org/dist/{version}/SHASUMS256.txt",
		List:      true,
		Strip:     1,
		Binary:    "bin/node",
		// npm ships with Node and verifies what it installs, so pnpm arrives
		// checked without this package having to know how npm does it.
		Then: [][]string{{"bin/npm", "install", "--global", "--prefix", ".", "pnpm"}},
		OS:   map[string]string{"darwin": "darwin", "linux": "linux"},
		Arch: map[string]string{"arm64": "arm64", "amd64": "x64"},
	},
}

// RecipeFor returns how to install the tool a language needs.
func RecipeFor(language string) (Toolchain, Recipe, error) {
	tc, ok := Lookup(language)
	if !ok {
		return Toolchain{}, Recipe{}, Unknown(language)
	}
	r, ok := recipes[tc.Tool]
	if !ok {
		return tc, Recipe{}, fmt.Errorf(
			"%s needs %s, and this engine has no verified way to install it.\n"+
				"Nothing is installed that cannot be checked against a published "+
				"checksum, and %s publishes none.\n"+
				"Install it yourself, and this worker will pick it up when it restarts.",
			language, tc.Tool, tc.Tool)
	}
	return tc, r, nil
}

// URLs renders a recipe's download and checksum locations for this machine.
func (r Recipe) URLs() (archive, checksums string, err error) {
	goos, ok := r.OS[runtime.GOOS]
	if !ok {
		return "", "", fmt.Errorf("no build published for %s", runtime.GOOS)
	}
	arch, ok := r.Arch[runtime.GOARCH]
	if !ok {
		return "", "", fmt.Errorf("no build published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sub := func(s string) string {
		s = strings.ReplaceAll(s, "{version}", r.Version)
		s = strings.ReplaceAll(s, "{os}", goos)
		return strings.ReplaceAll(s, "{arch}", arch)
	}
	return sub(r.Archive), sub(r.Checksums), nil
}

// Installable lists the languages with a verified recipe.
func Installable() []string {
	var out []string
	for _, t := range table {
		if _, ok := recipes[t.Tool]; ok {
			out = append(out, t.Name)
		}
	}
	return out
}
