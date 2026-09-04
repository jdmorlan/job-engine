package cli

import (
	"fmt"
	"sort"
	"strings"
)

// The languages `je new --language` can scaffold.
//
// This replaced a boolean `--script`, which wrote scripts/<name>.sh and a
// `/bin/sh` command. That was a shell-only special case sitting next to a
// definition field called `language:` -- the CLI had an opinion the format did
// not share, and adding a second language meant adding a second flag.
//
// It writes `language:` into the job file for a language the engine can
// actually prepare -- which is what that field means now: the worker installs
// dependencies from the repository's lockfile (D28) and materialises the
// helpers the job can import (D21). Not for `sh`, which has neither, and where
// the field would be a declaration with nothing behind it.
//
// This used to write it for nothing, because `language:` opted a job into shim
// injection before shim injection existed. Both halves are built now.
type scriptLanguage struct {
	name        string
	ext         string
	interpreter string
	body        func(name string) string
}

func (l scriptLanguage) set() bool { return l.name != "" }

func (l scriptLanguage) template(job string) string { return l.body(job) }

var languages = map[string]scriptLanguage{
	"sh": {
		name: "sh", ext: ".sh", interpreter: "/bin/sh",
		body: scriptTemplate,
	},
	"python": {
		name: "python", ext: ".py", interpreter: "python3",
		body: pythonTemplate,
	},
}

// resolveLanguage turns the flags into one answer, and refuses a combination
// that asks for two different things.
func resolveLanguage(env *Env, name string, legacyScript bool) (scriptLanguage, error) {
	switch {
	case name == "" && !legacyScript:
		return scriptLanguage{}, nil
	case name == "" && legacyScript:
		// --script shipped, so it keeps working. It means shell, which is what
		// it always wrote.
		fmt.Fprintln(env.Stderr, "note: --script is now --language sh")
		return languages["sh"], nil
	case legacyScript:
		return scriptLanguage{}, usagef("--script is the old name for --language sh; use one or the other")
	}

	lang, ok := languages[strings.ToLower(name)]
	if !ok {
		return scriptLanguage{}, usagef("no template for %q; there is one for %s",
			name, strings.Join(languageNames(), " and "))
	}
	return lang, nil
}

func languageNames() []string {
	out := make([]string, 0, len(languages))
	for name := range languages {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// pythonTemplate is the same contract in a different language, which is the
// point of it existing.
//
// D6 says the filesystem is the contract and there is nothing to import. A
// second template is the cheapest possible proof of that: it reads the same
// variables and writes the same three files, with no library in sight.
func pythonTemplate(name string) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
"""%s

The engine talks to this script through the environment and three files. There
is no SDK and nothing to import: the filesystem is the contract (D6), and
everything below works the same in any language.
"""
import datetime
import json
import os

# JE_STATE is your cursor, as JSON. On the very first run the engine seeds it
# with the run's start time, so it is never empty and never the epoch -- there
# is no recovering from having already hammered somebody's API for all history.
state = json.loads(os.environ.get("JE_STATE") or "{}")
print("cursor in: ", state)

# Also set: JOB_ID, RUN_ID, ATTEMPT, TRIGGERED_BY, EVENT_PAYLOAD, JOB_WORKDIR,
# and JE_LAST_SUCCESS_AT (absent if this job has never succeeded).
print("run {}, attempt {}, triggered by {}".format(
    os.environ["RUN_ID"], os.environ["ATTEMPT"], os.environ["TRIGGERED_BY"]))

# Anything printed is captured and stored, and is what %sje logs%s shows.
print("doing the work")

# Write the new cursor here. Not writing the file means "no change", which is a
# supported outcome rather than an error.
#
# The engine commits this ONLY if this script exits zero. That is the whole
# point of D14: a job that fails half way does not advance its watermark and
# silently skip the records it never processed.
now = datetime.datetime.now(datetime.timezone.utc).strftime("%%Y-%%m-%%dT%%H:%%M:%%SZ")
with open(os.environ["JOB_STATE_OUT_FILE"], "w") as f:
    json.dump({"since": now}, f)

# Structured output, for anything reading this run's result.
with open(os.environ["JOB_OUTPUT_FILE"], "w") as f:
    json.dump({"processed": 0}, f)

# Emit events, one JSON object per line. A chain step can trigger on these, so
# this is how one job sets off another without naming it.
with open(os.environ["JOB_EVENTS_FILE"], "a") as f:
    f.write(json.dumps({"type": "%s.finished", "payload": {"processed": 0}}) + "\n")
`, titleFromSlug(name), "`", "`", name)
}
