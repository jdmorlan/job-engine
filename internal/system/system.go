// Package system holds the engine's own work, defined as jobs (P2).
//
// > "I really like that this is another job and really agree with system
// > actions being jobs like everything else. Plus it grows out our trigger
// > library."
//
// Retention, and whatever follows it, are ordinary definitions in ordinary
// YAML. They appear in the job list, they have runs, they have logs, and they
// can fail visibly. P2 calls this a forcing function rather than decoration:
// if the engine's own work is awkward to express in our job format, the format
// has a hole, and we find it on ourselves first.
//
// The embedding is what makes this a source at all. D27 settled that only
// repositories are sources, on the argument that code which cannot travel is
// not a source -- a job whose script sat on the control plane's disk could
// never run on a worker anywhere else. These jobs run `je`, which is on every
// worker by definition, because C10 requires the worker to be the same version
// as the control plane. So this is the one tree that is already everywhere,
// and the rule it appears to break is the rule it satisfies most completely.
package system

import (
	"embed"

	"github.com/jdmorlan/job-engine/internal/jobdef"
)

//go:embed jobs
var files embed.FS

// Name is the source these jobs belong to, so they are called
// system/retention like every other job is called for its source (D22).
const Name = "system"

// Source is the engine's own definitions, read the same way a repository's are.
//
// jobdef.Source takes an fs.FS rather than a path, and its comment says why:
// "so the same code serves a directory, a test's fstest.MapFS, and eventually a
// git worktree, without any of them being a special case." An embedded tree is
// the case that interface was shaped for, and it costs nothing here.
func Source() jobdef.FSSource {
	return jobdef.FSSource{FS: files, Root: "jobs", Name: Name}
}
