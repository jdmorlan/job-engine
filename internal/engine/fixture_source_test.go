package engine_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/testsupport"
)

// fixtureTrees is where each engine's fixture definitions are edited.
//
// The working copy, not the cache: tests write here and sync, exactly as
// somebody commits and pushes. The cache is the engine's business.
var (
	fixtureMu    sync.Mutex
	fixtureTrees = map[*engine.Engine]string{}
	fixtureHubs  = map[*engine.Engine]*testsupport.GitHub{}
)

func rememberFixture(e *engine.Engine, dir string, hub *testsupport.GitHub) {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	fixtureTrees[e], fixtureHubs[e] = dir, hub
}

// treeDir is where a fixture's definitions are written and edited.
func treeDir(e *engine.Engine) string {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	return fixtureTrees[e]
}

func chainsDir(e *engine.Engine) string { return filepath.Join(treeDir(e), "chains") }

func hubFor(e *engine.Engine) *testsupport.GitHub {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	return fixtureHubs[e]
}

// newHub is testsupport.NewGitHub with the test's cleanup attached.
func newHub(t *testing.T) *testsupport.GitHub {
	t.Helper()
	hub := testsupport.NewGitHub()
	t.Cleanup(hub.Close)
	return hub
}
