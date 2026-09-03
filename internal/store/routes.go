package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Where a route was authored. A job-local `on:` block will compile to rows here
// too when job files grow inter-job triggers; today every route comes from a
// chain file.
//
// Distinct from the registered Source the rule arrived from (D22): one says
// which kind of file wrote it, the other says which repo that file was in.
const (
	AuthoredInChainFile = "chain_file"
	AuthoredInJobFile   = "job_file"
)

// Route is one trigger as the database holds it: an event pattern and the job
// it starts (D17).
//
// There is exactly one trigger table regardless of where the rule was
// authored, which is what lets one query answer "what fires this job?" without
// knowing whether somebody wrote it in a chain file or a job file.
type Route struct {
	ID          int64           `json:"id"`
	TargetJobID int64           `json:"target_job_id"`
	TargetSlug  string          `json:"target"`
	Match       json.RawMessage `json:"match"`
	RouteHash   string          `json:"route_hash"`
	ChainName   string          `json:"chain,omitempty"`
	StepIndex   int             `json:"step,omitempty"`

	// Source is the registered place this rule arrived from, and Authored is
	// which kind of file wrote it.
	Source   string `json:"source"`
	Authored string `json:"authored"`

	FilePath  string     `json:"file_path"`
	Enabled   bool       `json:"enabled"`
	LoadError string     `json:"load_error,omitempty"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

// Chain is the display grouping a route belongs to (D17). It is not a runtime
// entity: nothing here is consulted to decide whether anything runs.
type Chain struct {
	Name        string     `json:"name"`
	Source      string     `json:"source"`
	Description string     `json:"description,omitempty"`
	FilePath    string     `json:"file_path"`
	LoadedAt    time.Time  `json:"loaded_at"`
	RemovedAt   *time.Time `json:"removed_at,omitempty"`
}

// Removed reports whether this chain's file is gone.
func (c Chain) Removed() bool { return c.RemovedAt != nil }

// ReplaceRoutes projects the loaded chain files into the two tables, in one
// transaction.
//
// Whole-world rather than a diff, for the same reason Source.Load is (D19): a
// partially applied set of routes is wiring that exists in no commit. The
// tombstone-then-restore shape is what makes that atomic -- everything is
// marked removed, and every rule that is still in a file un-removes itself, so
// a rule that disappeared from a file stops firing at exactly the moment the
// transaction commits.
//
// Rows are never deleted. Runs record which route fired them (D11), so
// deleting a route would either fail against the foreign key or erase the
// provenance of everything it ever caused.
func (s *Store) ReplaceRoutes(ctx context.Context, source string, chains []Chain, routes []Route) error {
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after a successful Commit

	// Scoped to one source (D22). Loading the weather repo must not tombstone
	// the wiring the home repo installed, which a whole-table sweep would do
	// silently and completely.
	at := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx,
		`UPDATE routes SET removed_at = ? WHERE removed_at IS NULL AND source_name = ?`,
		at, source); err != nil {
		return fmt.Errorf("tombstoning routes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE chains SET removed_at = ? WHERE removed_at IS NULL AND source = ?`,
		at, source); err != nil {
		return fmt.Errorf("tombstoning chains: %w", err)
	}

	for _, c := range chains {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chains (name, source, description, file_path, loaded_at, removed_at)
			VALUES (?, ?, ?, ?, ?, NULL)
			ON CONFLICT (name) DO UPDATE SET
				source      = excluded.source,
				description = excluded.description,
				file_path   = excluded.file_path,
				loaded_at   = excluded.loaded_at,
				removed_at  = NULL`,
			c.Name, source, nullString(c.Description), c.FilePath, at,
		); err != nil {
			return fmt.Errorf("recording chain %s: %w", c.Name, err)
		}
	}

	for _, r := range routes {
		// (chain, step) is the rule's address in its file, which is what lets
		// a reload update it in place instead of inserting a duplicate on
		// every sync.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO routes (
				target_job_id, match, route_hash, chain_name, step_index,
				source, source_name, file_path, enabled, load_error, removed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT (chain_name, step_index) WHERE chain_name IS NOT NULL
			DO UPDATE SET
				target_job_id = excluded.target_job_id,
				source_name   = excluded.source_name,
				match         = excluded.match,
				route_hash    = excluded.route_hash,
				source        = excluded.source,
				file_path     = excluded.file_path,
				enabled       = excluded.enabled,
				load_error    = excluded.load_error,
				removed_at    = NULL`,
			r.TargetJobID, string(r.Match), r.RouteHash, nullString(r.ChainName), r.StepIndex,
			r.Authored, source, r.FilePath, r.Enabled, nullString(r.LoadError),
		); err != nil {
			return fmt.Errorf("recording route %s step %d: %w", r.ChainName, r.StepIndex, err)
		}
	}
	return tx.Commit()
}

const selectRoute = `
	SELECT r.id, r.target_job_id, j.name, r.match, r.route_hash,
	       r.chain_name, r.source_name, r.step_index, r.source, r.file_path,
	       r.enabled, r.load_error, r.removed_at
	FROM routes r
	JOIN jobs j ON j.id = r.target_job_id`

// ActiveRoutes returns every rule that could fire, in a stable order.
func (s *Store) ActiveRoutes(ctx context.Context) ([]Route, error) {
	return s.queryRoutes(ctx, selectRoute+`
		WHERE r.removed_at IS NULL AND r.enabled = 1 AND r.load_error IS NULL
		ORDER BY r.chain_name, r.step_index, r.id`)
}

// RoutesForChain returns one chain's rules in step order, including tombstoned
// ones, so a chain view can still explain a run that a since-deleted rule fired.
func (s *Store) RoutesForChain(ctx context.Context, chain string) ([]Route, error) {
	return s.queryRoutes(ctx, selectRoute+`
		WHERE r.chain_name = ? ORDER BY r.step_index`, chain)
}

func (s *Store) queryRoutes(ctx context.Context, query string, args ...any) ([]Route, error) {
	rows, err := s.state.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing routes: %w", err)
	}
	defer rows.Close()

	var out []Route
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRoute(sc scanner) (Route, error) {
	var (
		r         Route
		match     string
		chainName sql.NullString
		stepIndex sql.NullInt64
		loadErr   sql.NullString
		removedAt sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.TargetJobID, &r.TargetSlug, &match, &r.RouteHash,
		&chainName, &r.Source, &stepIndex, &r.Authored, &r.FilePath, &r.Enabled, &loadErr,
		&removedAt); err != nil {
		return Route{}, err
	}
	r.Match = json.RawMessage(match)
	r.ChainName = chainName.String
	r.StepIndex = int(stepIndex.Int64)
	r.LoadError = loadErr.String

	var err error
	if r.RemovedAt, err = parseNullTime(removedAt); err != nil {
		return Route{}, fmt.Errorf("route %d: %w", r.ID, err)
	}
	return r, nil
}

const selectChain = `
	SELECT name, description, file_path, loaded_at, removed_at FROM chains`

// ListChains returns every chain that still has a file, by name.
func (s *Store) ListChains(ctx context.Context) ([]Chain, error) {
	rows, err := s.state.QueryContext(ctx,
		selectChain+` WHERE removed_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing chains: %w", err)
	}
	defer rows.Close()

	var out []Chain
	for rows.Next() {
		c, err := scanChain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChainByName looks up one chain. Returns sql.ErrNoRows if there is no such chain.
func (s *Store) ChainByName(ctx context.Context, name string) (Chain, error) {
	return scanChain(s.state.QueryRowContext(ctx, selectChain+` WHERE name = ?`, name))
}

func scanChain(sc scanner) (Chain, error) {
	var (
		c           Chain
		description sql.NullString
		loadedAt    string
		removedAt   sql.NullString
	)
	if err := sc.Scan(&c.Name, &description, &c.FilePath, &loadedAt, &removedAt); err != nil {
		return Chain{}, err
	}
	c.Description = description.String

	var err error
	if c.RemovedAt, err = parseNullTime(removedAt); err != nil {
		return Chain{}, fmt.Errorf("chain %s: %w", c.Name, err)
	}
	if c.LoadedAt, err = parseTime(loadedAt); err != nil {
		return Chain{}, fmt.Errorf("chain %s has unparseable loaded_at %q: %w", c.Name, loadedAt, err)
	}
	return c, nil
}
