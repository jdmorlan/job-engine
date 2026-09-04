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

	// project is files the language needs before a job in it can run at all,
	// written only when they are not already there.
	project map[string]string

	// next is anything the author has to do by hand before `je try` works.
	next []string
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
	// The two that get the shim (D21), and the reason this map was missing the
	// language with the best experience in it: `language: typescript` shipped
	// with helpers to import, and `je new` could not scaffold one.
	"javascript": {
		name: "javascript", ext: ".mjs", interpreter: "node",
		body: javascriptTemplate,
	},
	"typescript": {
		name: "typescript", ext: ".ts", interpreter: "tsx",
		body: typescriptTemplate,

		// TypeScript is the one that cannot run on what is already installed.
		// `tsx` comes from the tree's own dependencies, so the scaffold has to
		// write the manifest that declares it and say what to run -- the
		// alternative is a job file that looks finished and fails on the first
		// `je try` with "command not found".
		project: map[string]string{"package.json": nodePackageJSON},
		next: []string{
			"pnpm install\twrites the lockfile the worker installs from",
		},
	},
}

// nodePackageJSON is the smallest project that can run a TypeScript job.
//
// tsx rather than a build step: a job is a script, and asking somebody to
// compile one before the engine will run it would put a build directory
// between the file they edited and the thing that ran.
const nodePackageJSON = `{
  "name": "jobs",
  "private": true,
  "type": "module",
  "devDependencies": {
    "tsx": "^4.19.2"
  }
}
`

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
			name, andList(languageNames()))
	}
	return lang, nil
}

// andList reads the way somebody would say it. Four items joined by "and" is
// how you can tell nobody looked at the output.
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
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

// javascriptTemplate is the same contract through the shim (D21).
//
// The other templates exist to prove D6: the filesystem is the contract and
// there is nothing to import. This one exists to show the other half -- that
// where we ship a helper, it is sugar over exactly those files and never more.
// Both are true at once, and a reader should be able to see it.
func javascriptTemplate(name string) string {
	return fmt.Sprintf(`// %s
//
// `+"`import je from \"je\"`"+` is written into this tree by the worker that runs
// the job -- there is nothing to install and no version to match, because the
// helper is materialised by the same binary that runs you (D21).
//
// It is sugar. Every line below reads an environment variable the engine set or
// writes one of three files it reads back, which is why a job in a language we
// ship no helper for can do all of it by hand (D6).
import je from "je";

// Your cursor. On the very first run the engine seeds it with the run's start
// time, so it is never empty and never the epoch -- there is no recovering from
// having already hammered somebody's API for all of history.
console.log("cursor in:", je.state);

// The event that caused this run, and when this job last succeeded.
console.log("triggered by an event with", Object.keys(je.event).length, "field(s)");
console.log("last success:", je.lastSuccessAt ?? "never");

// Anything printed is captured and stored, and is what `+"`je logs`"+` shows.
console.log("doing the work");

// The engine commits this ONLY if the job exits zero. That is the whole point
// of D14: a job that fails half way does not advance its watermark and silently
// skip the records it never processed.
je.setState({ since: new Date().toISOString() });

// Structured output, for anything reading this run's result.
je.output({ processed: 0 });

// Events other jobs can be triggered by, which is how one job sets off another
// without naming it (D17).
je.emit("%s.finished", { processed: 0 });
`, titleFromSlug(name), name)
}

// typescriptTemplate is javascriptTemplate with the types a reader would expect
// to see, and nothing else different.
func typescriptTemplate(name string) string {
	return fmt.Sprintf(`// %s
//
// `+"`import je from \"je\"`"+` is written into this tree by the worker that runs
// the job -- there is nothing to install and no version to match, because the
// helper is materialised by the same binary that runs you (D21).
//
// It is sugar. Every line below reads an environment variable the engine set or
// writes one of three files it reads back, which is why a job in a language we
// ship no helper for can do all of it by hand (D6).
import je from "je";

type Cursor = { since?: string };

// Your cursor. On the very first run the engine seeds it with the run's start
// time, so it is never empty and never the epoch -- there is no recovering from
// having already hammered somebody's API for all of history.
const cursor = je.state as Cursor;
console.log("cursor in:", cursor);

// The event that caused this run, and when this job last succeeded.
console.log("triggered by an event with", Object.keys(je.event).length, "field(s)");
console.log("last success:", je.lastSuccessAt ?? "never");

// Anything printed is captured and stored, and is what `+"`je logs`"+` shows.
console.log("doing the work");

// The engine commits this ONLY if the job exits zero. That is the whole point
// of D14: a job that fails half way does not advance its watermark and silently
// skip the records it never processed.
je.setState({ since: new Date().toISOString() } satisfies Cursor);

// Structured output, for anything reading this run's result.
je.output({ processed: 0 });

// Events other jobs can be triggered by, which is how one job sets off another
// without naming it (D17).
je.emit("%s.finished", { processed: 0 });
`, titleFromSlug(name), name)
}
