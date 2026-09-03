package jobdef

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"crypto/sha256"
	"encoding/hex"
)

// Chain is one flow, and the whole of D17.
//
// A chain is how routes are named and grouped; routes are still what executes.
// The steps fire independently as ordinary triggers -- there is no chain-level
// execution, no chain state machine, no "the chain is running" lock. The moment
// a chain becomes a runtime object with its own lifecycle we have built a DAG
// engine with extra steps, which is the thing this project exists not to be.
//
// What the name buys is entirely visibility, and that is enough: end-to-end
// duration and end-to-end failure are expressible for a named flow and are not
// expressible for anonymous rules.
type Chain struct {
	// Name comes from the file name, not the file's contents -- the same rule
	// as a job's slug, so renaming a chain is a file rename and shows in git.
	Name string `json:"name"`

	Description string `json:"description,omitempty"`
	Steps       []Step `json:"steps"`

	filePath string
}

// Step is one route: an event pattern, and the job to run when it matches.
type Step struct {
	On Match `json:"on"`

	// Run is the slug of the job this step starts. It is resolved against the
	// jobs in the same source at load time, so a typo is a load error rather
	// than a rule that silently never fires (D10's pit of success).
	Run string `json:"run"`
}

// Match is an event pattern.
//
// Deliberately not an expression language. D3 bounds this at equality on
// top-level payload fields, because the moment we ship a DSL we have built a
// workflow language by accident. Extend it when a real job needs more.
type Match struct {
	Event string `json:"event"`

	// Where compares top-level fields of the event payload for equality.
	// Values are normalised to strings on both sides, so `where: {count: 3}`
	// matches a payload of {"count": 3}.
	Where map[string]string `json:"where,omitempty"`
}

// FilePath reports where this chain was read from.
func (c *Chain) FilePath() string { return c.filePath }

// MatchJSON renders a step's pattern as the JSON stored in routes.match.
//
// encoding/json sorts map keys lexically, so this is deterministic without a
// canonicalisation pass -- which matters because it is what gets hashed.
func (s Step) MatchJSON() ([]byte, error) { return json.Marshal(s.On) }

// RouteHash identifies the rule, for the run rows that record which rule fired
// them (D11).
//
// It covers the match and the target and deliberately not the step's position:
// reordering the steps in a file does not make them different rules, and a run
// that says "fired by this route" should keep pointing at the same rule when
// somebody inserts a step above it.
func (s Step) RouteHash() (string, error) {
	body, err := json.Marshal(struct {
		On  Match  `json:"on"`
		Run string `json:"run"`
	}{s.On, s.Run})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16]), nil
}

// Matches reports whether an event satisfies this pattern.
//
// payload is the event's payload as stored. A pattern with no `where` matches
// every event of its type, including one with no payload at all.
func (m Match) Matches(eventType string, payload json.RawMessage) bool {
	if m.Event != eventType {
		return false
	}
	if len(m.Where) == 0 {
		return true
	}

	fields := map[string]json.RawMessage{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &fields); err != nil {
			// A payload that is not an object cannot satisfy a field
			// comparison. Not an error: `where` simply does not match.
			return false
		}
	}
	for key, want := range m.Where {
		raw, ok := fields[key]
		if !ok {
			return false
		}
		got, ok := scalarFromJSON(raw)
		if !ok || got != want {
			return false
		}
	}
	return true
}

// scalarFromJSON renders a payload field as the string `where` compares
// against. Objects and arrays report false: they are not things equality on a
// field is asking about.
func scalarFromJSON(raw json.RawMessage) (string, bool) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber() // so 3 stays "3" rather than becoming "3e+00"
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	default:
		return "", false
	}
}

// rawChain mirrors the YAML shape. KnownFields is enforced on decode, so a
// typo'd key is an error rather than wiring that silently does nothing --
// which for a chain file is the worst possible failure, since the symptom is
// "the second job never runs" and there is nothing to look at.
type rawChain struct {
	Description *string   `yaml:"description"`
	Steps       []rawStep `yaml:"steps"`
}

type rawStep struct {
	On  *rawMatch `yaml:"on"`
	Run *string   `yaml:"run"`
}

type rawMatch struct {
	Event *string        `yaml:"event"`
	Where map[string]any `yaml:"where"`

	// Fan-in, D3's v1.1 shape. Named here rather than left to KnownFields so
	// that writing it gets "not implemented yet" and a decision number instead
	// of "field all_of not found in type jobdef.rawMatch".
	AllOf  []rawMatch `yaml:"all_of"`
	Within *Duration  `yaml:"within"`
	Fire   *string    `yaml:"fire"`
}

// ParseChain reads one chain file. name comes from the file name.
func ParseChain(path, name string, body []byte) (*Chain, error) {
	raw, err := decodeKnownFields[rawChain](path, body)
	if err != nil {
		return nil, err
	}

	c := &Chain{Name: name, filePath: path}
	if raw.Description != nil {
		c.Description = *raw.Description
	}

	for i, rs := range raw.Steps {
		step, err := rs.step()
		if err != nil {
			return nil, fmt.Errorf("%s: step %d: %w", path, i+1, err)
		}
		c.Steps = append(c.Steps, step)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (rs rawStep) step() (Step, error) {
	var s Step
	if rs.On == nil {
		return Step{}, errors.New("needs an `on:` pattern")
	}
	switch {
	case len(rs.On.AllOf) > 0:
		return Step{}, errors.New(
			"all_of (fan-in) is not implemented yet (D3) -- one condition per step for now")
	case rs.On.Within != nil || rs.On.Fire != nil:
		return Step{}, errors.New(
			"within/fire belong to all_of (fan-in), which is not implemented yet (D3)")
	case rs.On.Event == nil || *rs.On.Event == "":
		return Step{}, errors.New("on.event is required")
	case rs.Run == nil || *rs.Run == "":
		return Step{}, errors.New("needs `run:`, the job to start")
	}

	s.On.Event = *rs.On.Event
	s.Run = *rs.Run
	if len(rs.On.Where) > 0 {
		s.On.Where = make(map[string]string, len(rs.On.Where))
		for key, value := range rs.On.Where {
			text, ok := scalarFromYAML(value)
			if !ok {
				return Step{}, fmt.Errorf(
					"on.where.%s must be a string, number or boolean -- "+
						"`where` compares fields for equality, nothing more (D3)", key)
			}
			s.On.Where[key] = text
		}
	}
	return s, nil
}

func scalarFromYAML(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	default:
		return "", false
	}
}

// Validate reports the first thing wrong with a chain.
func (c *Chain) Validate() error {
	if !slugPattern.MatchString(c.Name) {
		return fmt.Errorf("chain name %q must be lowercase letters, digits and dashes", c.Name)
	}
	if len(c.Steps) == 0 {
		return errors.New("a chain needs at least one step")
	}
	for i, s := range c.Steps {
		if !slugPattern.MatchString(s.Run) {
			return fmt.Errorf("step %d: run: %q is not a job name", i+1, s.Run)
		}
	}
	return nil
}

// ChainNameFromPath derives a chain's identity from its file name.
func ChainNameFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// runEventTypes are the event types whose payload names the job that produced
// them, which is what makes a chain's shape statically knowable.
//
// A step matching one of these is an edge from that job to the step's target.
// A step matching a job-emitted event is not, because nothing in the files says
// which job emits `weather.ingested` -- that is why the runtime depth guard
// stays even though the static check exists.
var runEventTypes = map[string]bool{
	"run.succeeded": true,
	"run.failed":    true,
	"run.skipped":   true,
}

// checkCycles rejects a set of chains that wires a job back to itself.
//
// Static rejection at load time is worth much more than the runtime depth
// guard: a cycle is never intentional, and finding it when the file is saved
// costs a message, while finding it at 3am costs ten runs of the wrong thing
// before the guard trips.
func checkCycles(chains []*Chain) error {
	type edge struct {
		to   string
		file string
		step int
	}
	graph := map[string][]edge{}
	for _, c := range chains {
		for i, s := range c.Steps {
			if !runEventTypes[s.On.Event] {
				continue
			}
			from := s.On.Where["job"]
			if from == "" {
				continue
			}
			graph[from] = append(graph[from], edge{to: s.Run, file: c.FilePath(), step: i + 1})
		}
	}

	// Sorted so a cycle in a set of files is reported the same way every time.
	roots := make([]string, 0, len(graph))
	for from := range graph {
		roots = append(roots, from)
	}
	sort.Strings(roots)

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := map[string]int{}

	var walk func(job string, path []string) error
	walk = func(job string, path []string) error {
		state[job] = onStack
		path = append(path, job)
		for _, e := range graph[job] {
			switch state[e.to] {
			case onStack:
				return fmt.Errorf(
					"%s: step %d closes a loop: %s -- a job that triggers itself "+
						"never stops, so this is refused at load time (D3)",
					e.file, e.step, strings.Join(append(path, e.to), " -> "))
			case unvisited:
				if err := walk(e.to, path); err != nil {
					return err
				}
			}
		}
		state[job] = done
		return nil
	}

	for _, from := range roots {
		if state[from] == unvisited {
			if err := walk(from, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
