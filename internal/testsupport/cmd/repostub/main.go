// Command repostub serves a directory as a GitHub repository, so a real `je`
// can be pointed at it with --github-api.
//
// It exists because every source is a repository now (D22): verifying anything
// by hand -- a TypeScript job's dependencies being installed, a sync picking up
// a change -- needs something for the control plane to fetch from, and reaching
// for a real GitHub repo to test a local change is a poor loop.
//
//	repostub you/house ./my-jobs
//	je up --foreground --github-api <the URL it prints>
package main

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/jdmorlan/job-engine/internal/testsupport"
)

func main() {
	hub := testsupport.NewGitHub()
	defer hub.Close()
	hub.Add(os.Args[1], os.Args[2])
	fmt.Println(hub.URL)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}
