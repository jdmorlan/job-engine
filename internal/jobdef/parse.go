package jobdef

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// rawDefinition mirrors the YAML shape. It is separate from Definition so that
// "was this field written down?" stays answerable: every field here is a
// pointer or a slice, so nil means absent and a zero value means the author
// deliberately wrote a zero.
//
// KnownFields is enforced on decode, so a typo'd key is an error rather than a
// setting that silently does nothing -- the worst failure mode a config format
// has.
type rawDefinition struct {
	Name        *string       `yaml:"name"`
	Description *string       `yaml:"description"`
	Command     []string      `yaml:"command"`
	Workdir     *string       `yaml:"workdir"`
	Runtime     *Runtime      `yaml:"runtime"`
	RunsOn      *string       `yaml:"runs_on"`
	Language    *string       `yaml:"language"`
	Timeout     *Duration     `yaml:"timeout"`
	Overlap     *Overlap      `yaml:"overlap"`
	OnInterrupt *OnInterrupt  `yaml:"on_interrupt"`
	Enabled     *bool         `yaml:"enabled"`
	On          []rawSchedule `yaml:"on"`
	State       *rawState     `yaml:"state"`
	Secrets     []string      `yaml:"secrets"`
}

type rawSchedule struct {
	Every    *Duration `yaml:"every"`
	Cron     *string   `yaml:"cron"`
	Timezone *string   `yaml:"timezone"`
	CatchUp  *CatchUp  `yaml:"catch_up"`
}

type rawState struct {
	PrimaryCursor *string      `yaml:"primary_cursor"`
	Commit        *StateCommit `yaml:"commit"`
}

// Parse reads one job definition. slug comes from the file name, not the file.
func Parse(path, slug string, body []byte) (*Definition, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// An empty file decodes to a zero Node rather than an error, which would
	// otherwise surface much later as "command is required" on a file the
	// author thinks they filled in.
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: file is empty", path)
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: expected a mapping at the top level", path)
	}

	// Decoded separately from the Node walk above, because KnownFields lives
	// on the Decoder and not on Node.Decode. Worth the second pass: without it
	// a misspelled key is silently ignored, which is the worst failure mode a
	// config format has -- the file looks right and the setting does nothing.
	var raw rawDefinition
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	d := &Definition{
		Slug:     slug,
		filePath: path,
		declared: declaredLines(root),

		// Defaults first, then anything the author actually wrote. Written this
		// way round so that adding a field means adding one default and one
		// override, and forgetting the override fails a test rather than
		// producing a silent zero value.
		Runtime:     DefaultRuntime,
		RunsOn:      DefaultRunsOn,
		Timeout:     Duration{DefaultTimeout},
		Overlap:     DefaultOverlap,
		OnInterrupt: DefaultOnInterrupt,
		Enabled:     true,
		State: StateSpec{
			PrimaryCursor: DefaultPrimaryCursor,
			Commit:        DefaultStateCommit,
		},
	}

	setIf(&d.DisplayName, raw.Name)
	setIf(&d.Description, raw.Description)
	setIf(&d.Workdir, raw.Workdir)
	setIf(&d.Runtime, raw.Runtime)
	setIf(&d.RunsOn, raw.RunsOn)
	setIf(&d.Language, raw.Language)
	setIf(&d.Timeout, raw.Timeout)
	setIf(&d.Overlap, raw.Overlap)
	setIf(&d.OnInterrupt, raw.OnInterrupt)
	setIf(&d.Enabled, raw.Enabled)
	d.Command = raw.Command
	d.Secrets = raw.Secrets

	if raw.State != nil {
		setIf(&d.State.PrimaryCursor, raw.State.PrimaryCursor)
		setIf(&d.State.Commit, raw.State.Commit)
	}

	for _, rs := range raw.On {
		s := Schedule{CatchUp: DefaultCatchUp}
		setIf(&s.Every, rs.Every)
		setIf(&s.Cron, rs.Cron)
		setIf(&s.Timezone, rs.Timezone)
		setIf(&s.CatchUp, rs.CatchUp)
		d.Schedules = append(d.Schedules, s)
	}

	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// setIf assigns only when the author wrote something, leaving the default in
// place otherwise.
func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// declaredLines records the line number of each top-level key, which is what
// lets `je explain` cite jobs/weather-ingest.yaml:5 rather than just saying a
// value came from the file (P3).
func declaredLines(root *yaml.Node) map[string]int {
	lines := make(map[string]int, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		lines[root.Content[i].Value] = root.Content[i].Line
	}
	return lines
}

// decodeKnownFields decodes one YAML document, refusing unknown keys.
//
// Shared by jobs and chains because the reason is the same for both: without
// KnownFields a misspelled key is silently ignored, and the file looks right
// while the setting does nothing.
func decodeKnownFields[T any](path string, body []byte) (T, error) {
	var out T
	if len(bytes.TrimSpace(body)) == 0 {
		return out, fmt.Errorf("%s: file is empty", path)
	}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}
