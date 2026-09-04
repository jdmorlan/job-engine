package store

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/paths"
	_ "modernc.org/sqlite"
)

// D13's first requirement, and the one everybody assumes is free: deleting
// rows has to give the disk back.
//
// It does not, on its own. SQLite puts freed pages on a freelist and reuses
// them, so a sweep that only deleted would stop the file growing and never
// return a byte -- which is not what this database's own schema comment
// promises, and not what somebody watching a disk fill up means by retention.

// fillLogs writes n lines of a realistic width.
func fillLogs(t *testing.T, s *Store, n int) {
	t.Helper()
	line := ""
	for len(line) < 400 {
		line += "the quick brown fox jumps over the lazy dog; "
	}
	const batch = 2000
	for start := 0; start < n; start += batch {
		lines := make([]LogLine, 0, batch)
		for i := start; i < start+batch && i < n; i++ {
			lines = append(lines, LogLine{
				RunID: int64(i/1000 + 1), Attempt: 1, Seq: int64(i),
				Stream: StreamStdout, TS: time.Now(), Line: line,
			})
		}
		if err := s.AppendLogs(context.Background(), lines); err != nil {
			t.Fatalf("appending logs: %v", err)
		}
	}
}

func TestReclaimingLogSpaceGivesTheDiskBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	layout := paths.Layout{Data: dir}
	s, err := Open(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fillLogs(t, s, 40000)
	if _, err := s.logs.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	full := fileSize(layout.LogsDB())
	if full < 1<<20 {
		t.Fatalf("the fixture is too small to say anything: %d bytes", full)
	}

	if _, err := s.logs.ExecContext(ctx, `DELETE FROM logs WHERE seq < 30000`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.logs.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	// The point of the test: three quarters of the rows are gone and the file
	// is exactly as big as it was.
	if afterDelete := fileSize(layout.LogsDB()); afterDelete < full {
		t.Logf("note: deleting alone shrank the file from %d to %d", full, afterDelete)
	}

	space, err := s.ReclaimLogSpace(ctx, 0)
	if err != nil {
		t.Fatalf("ReclaimLogSpace: %v", err)
	}
	if space.Converted {
		t.Error("a database created by Open needed converting; it should be born incremental")
	}
	if space.PagesFreed == 0 {
		t.Fatal("nothing was reclaimed")
	}
	if space.PagesLeft != 0 {
		t.Errorf("%d pages left on the freelist after an uncapped reclaim", space.PagesLeft)
	}
	if space.BytesAfter >= space.BytesBefore {
		t.Fatalf("the file did not shrink: %d -> %d", space.BytesBefore, space.BytesAfter)
	}
	// Both sizes must be the same measurement. Taking the first before a
	// checkpoint and the second after it counted the write-ahead log being
	// folded in as part of the reclaim, and reported a negative number on a
	// database it had just tidied.
	if space.Reclaimed() <= 0 {
		t.Fatalf("reclaimed %d bytes, which is not a reclaim", space.Reclaimed())
	}
	// Deleting 3 of every 4 rows should return most of the file, not a token.
	if space.Reclaimed() < full/2 {
		t.Errorf("reclaimed only %d of a %d byte file", space.Reclaimed(), full)
	}

	// The rows that survived must still be readable, which is the thing a
	// vacuum could plausibly break.
	if n := count(t, s.logs, "logs"); n != 10000 {
		t.Fatalf("logs = %d after reclaiming, want 10000", n)
	}
	t.Logf("reclaimed %d KB of %d KB across %d pages",
		space.Reclaimed()/1024, full/1024, space.PagesFreed)
}

// A database from before D13 was created without auto_vacuum, and
// incremental_vacuum on one of those is a silent no-op: it returns in
// microseconds having freed nothing, because the pages it would move have no
// pointer map. Converting it is a one-time full rewrite, and it has to be
// reported rather than done quietly (P1).
func TestADatabaseFromBeforeD13IsConvertedOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	layout := paths.Layout{Data: dir}

	// A logs database exactly as an older version would have left it: same
	// schema, same pragmas, no auto_vacuum.
	old := openLegacyLogsDB(t, layout.LogsDB())
	if err := migrateDB(old, "migrations/logs"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Opening it does not convert it -- the DSN pragma is a no-op on a
	// database that already has tables, which is why this is not free.
	if mode, err := s.pragmaInt(ctx, "auto_vacuum"); err != nil {
		t.Fatal(err)
	} else if mode == autoVacuumIncremental {
		t.Fatal("an existing database converted itself on open, which SQLite cannot do")
	}

	fillLogs(t, s, 20000)
	if _, err := s.logs.ExecContext(ctx, `DELETE FROM logs WHERE seq < 15000`); err != nil {
		t.Fatal(err)
	}

	space, err := s.ReclaimLogSpace(ctx, 0)
	if err != nil {
		t.Fatalf("ReclaimLogSpace: %v", err)
	}
	if !space.Converted {
		t.Fatal("a pre-D13 database was not converted, so every future sweep would reclaim nothing")
	}
	if mode, err := s.pragmaInt(ctx, "auto_vacuum"); err != nil {
		t.Fatal(err)
	} else if mode != autoVacuumIncremental {
		t.Fatalf("auto_vacuum = %d after converting, want %d", mode, autoVacuumIncremental)
	}
	if n := count(t, s.logs, "logs"); n != 5000 {
		t.Fatalf("logs = %d after converting, want 5000", n)
	}

	// And the conversion is once: the next sweep is an ordinary reclaim.
	again, err := s.ReclaimLogSpace(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Converted {
		t.Error("the database was converted twice, so every sweep pays for a full VACUUM")
	}
	if again.Reclaimed() < 0 {
		t.Errorf("a second sweep reported reclaiming %d bytes", again.Reclaimed())
	}
}

func TestReclaimingRespectsItsBudget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	layout := paths.Layout{Data: dir}
	s, err := Open(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fillLogs(t, s, 40000)
	if _, err := s.logs.ExecContext(ctx, `DELETE FROM logs WHERE seq < 35000`); err != nil {
		t.Fatal(err)
	}
	space, err := s.ReclaimLogSpace(ctx, reclaimChunk)
	if err != nil {
		t.Fatal(err)
	}
	// A budget is a promise about how long the sweep holds the write lock, so
	// it has to stop short and say what is left rather than finishing the job.
	if space.PagesFreed > reclaimChunk {
		t.Errorf("freed %d pages on a budget of %d", space.PagesFreed, reclaimChunk)
	}
	if space.PagesLeft == 0 {
		t.Error("the budget was not reached, so this test proves nothing")
	}
}

// openLegacyLogsDB opens a logs database the way every version before D13 did:
// the same pragmas, minus auto_vacuum.
func openLegacyLogsDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)", "busy_timeout(5000)",
		"foreign_keys(1)", "synchronous(NORMAL)",
	} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}
