package engine

import (
	"sort"
	"strings"

	"github.com/jdmorlan/job-engine/internal/secrets"
)

// newRedactor builds a replacer that strips secret values from log lines.
//
// D10 redacts on write rather than at render: the value never reaches the
// database, so copying the log file later cannot leak it. This is imperfect by
// construction -- a job that base64s its token defeats it -- and it is aimed at
// the common case of a tool echoing its own configuration.
//
// Longest values are replaced first, so a secret that contains another secret
// as a prefix does not leave a fragment behind.
//
// D20 moved capture to the worker and left this here, which is the important
// half: the worker sends what the process printed, and redaction happens on the
// side that owns the database. A buggy or compromised worker cannot put a
// secret into the permanent record, and it never sees a value it was not given
// for the job it is running.
func newRedactor(values map[string]string) *strings.Replacer {
	if len(values) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= secrets.MinRedactableLength {
			ordered = append(ordered, v)
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })

	pairs := make([]string, 0, len(ordered)*2)
	for _, v := range ordered {
		pairs = append(pairs, v, "***")
	}
	return strings.NewReplacer(pairs...)
}
