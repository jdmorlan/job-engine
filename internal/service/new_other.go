//go:build !darwin && !linux

package service

import (
	"fmt"
	"runtime"
)

// New reports that this platform has no service integration.
//
// An honest refusal rather than a broken registration: running it in the
// foreground works everywhere, and telling somebody to supervise it themselves
// is better than writing a unit file nothing will read.
func New(c Component) (Manager, error) {
	return nil, fmt.Errorf(
		"no service integration for %s yet; run `je %s run` under whatever supervises processes here",
		runtime.GOOS, c)
}
