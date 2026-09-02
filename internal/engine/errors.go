package engine

import (
	"database/sql"
	"errors"
)

// isNoRows keeps the store's sql.ErrNoRows from becoming a fact the rest of
// the engine has to know. "There is no such row" is a normal answer here --
// a first-ever start has no history -- not an error to propagate.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
