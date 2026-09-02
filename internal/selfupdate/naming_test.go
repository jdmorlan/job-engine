package selfupdate_test

import (
	"os"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/selfupdate"
)

// TestAssetNameFormat pins the release filename.
//
// Three things have to agree on it and they are written in three languages:
// AssetName here, the tar command in .github/workflows/release.yml, and the
// ASSET variable in install.sh. The way they stop agreeing is by one of them
// being edited alone, and the symptom is a 404 during somebody's install --
// discovered by a stranger rather than by us.
func TestAssetNameFormat(t *testing.T) {
	got := selfupdate.AssetName("v1.2.3", "darwin", "arm64")
	if want := "je_1.2.3_darwin_arm64.tar.gz"; got != want {
		t.Fatalf("AssetName = %q, want %q", got, want)
	}
	// The leading v is stripped, because the tag is v1.2.3 and the filename is
	// conventionally not.
	if selfupdate.AssetName("1.2.3", "darwin", "arm64") != got {
		t.Error("AssetName should treat 1.2.3 and v1.2.3 as the same release")
	}
}

// TestShellScriptsUseTheSameName checks the two shell copies against this one.
//
// A brittle test in exchange for catching a failure that is otherwise only
// visible in production. If the shell is restructured, update the expectations
// here rather than deleting the test -- the drift it guards against is real.
func TestShellScriptsUseTheSameName(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"../../install.sh", `je_${BARE}_${OS}_${ARCH}.tar.gz`},
		{"../../.github/workflows/release.yml", `je_${bare}_${goos}_${goarch}.tar.gz`},
	} {
		body, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("reading %s: %v", tc.path, err)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s does not build the asset name as %s;\n"+
				"it must match selfupdate.AssetName or downloads will 404",
				tc.path, tc.want)
		}
	}
}

// TestChecksumsFileNameMatches guards the same drift for the manifest.
func TestChecksumsFileNameMatches(t *testing.T) {
	for _, path := range []string{"../../install.sh", "../../.github/workflows/release.yml"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), selfupdate.ChecksumsName) {
			t.Errorf("%s does not reference %s", path, selfupdate.ChecksumsName)
		}
	}
}
