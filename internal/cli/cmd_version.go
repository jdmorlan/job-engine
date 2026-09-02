package cli

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
)

func init() {
	register(&Command{
		Name:  "version",
		Usage: "print the version of this binary",
		Run:   runVersion,
	})
}

func runVersion(ctx context.Context, env *Env, args []string) error {
	fmt.Fprintf(env.Stdout, "je %s %s/%s\n", env.Version, runtime.GOOS, runtime.GOARCH)

	// The VCS stamp comes from the go tool automatically when building inside
	// a repository, so a binary built without a release process can still say
	// exactly which commit it is. Worth having from day one: "which version is
	// that daemon" is a question you ask at 2am, and D20 C10 will later refuse
	// version skew outright.
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					modified = " (dirty)"
				}
			}
		}
		if revision != "" {
			fmt.Fprintf(env.Stdout, "commit %s%s\n", revision, modified)
		}
	}
	return nil
}
