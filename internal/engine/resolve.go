package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jdmorlan/job-engine/internal/store"
)

// The qualified name is what the database stores, what events carry, and what
// chains compile to. The short form is a convenience that resolves here, at the
// edge -- the same way P3 has you write intent and the tool render truth (D22).
//
// Resolution is deliberately not a fallback chain with a winner. Two sources
// offering the same short name is an ambiguity, and picking one would mean
// `je run sync` quietly running the wrong repo's job.

// resolveJob turns whatever the user typed into exactly one job.
func (e *Engine) resolveJob(ctx context.Context, name string) (store.Job, error) {
	job, err := e.store.JobBySlug(ctx, name)
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.Job{}, err
	}
	// A name that already carries a source is exact or it is nothing: falling
	// back to a short-name search for "weather/ingest" could match a job in a
	// different source, which is the opposite of what qualifying it asked for.
	if strings.Contains(name, "/") {
		return store.Job{}, err
	}

	jobs, err := e.store.ListJobs(ctx)
	if err != nil {
		return store.Job{}, err
	}
	var matches []store.Job
	for _, j := range jobs {
		if _, slug := store.SourceOfName(j.Slug); slug == name && !j.Removed() {
			matches = append(matches, j)
		}
	}

	switch len(matches) {
	case 0:
		return store.Job{}, sql.ErrNoRows
	case 1:
		return matches[0], nil
	default:
		return store.Job{}, ambiguous("job", name, jobNames(matches))
	}
}

// resolveChain is the same rule for chains, which are named the same way.
func (e *Engine) resolveChain(ctx context.Context, name string) (store.Chain, error) {
	chain, err := e.store.ChainByName(ctx, name)
	if err == nil {
		return chain, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || strings.Contains(name, "/") {
		return store.Chain{}, err
	}

	chains, err := e.store.ListChains(ctx)
	if err != nil {
		return store.Chain{}, err
	}
	var matches []store.Chain
	for _, c := range chains {
		if _, slug := store.SourceOfName(c.Name); slug == name {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 0:
		return store.Chain{}, sql.ErrNoRows
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, c := range matches {
			names = append(names, c.Name)
		}
		return store.Chain{}, ambiguous("chain", name, names)
	}
}

// ambiguous is a resolution failure, which is a friendlier thing than a load
// failure: nothing is broken, the engine simply does not know which one you
// meant, and it can say exactly what to type instead.
func ambiguous(what, name string, candidates []string) error {
	sort.Strings(candidates)
	return fmt.Errorf("%q is ambiguous: %s -- name the source, e.g. %s",
		name, strings.Join(candidates, " and "), candidates[0])
}

func jobNames(jobs []store.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Slug)
	}
	return out
}
