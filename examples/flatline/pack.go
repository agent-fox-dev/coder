package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	afspec "github.com/agent-fox-dev/spec"
)

// Pack is the loaded spec plus the two things the library does not carry: the
// project root it belongs to and the sections of the project that the prompt
// needs (steering, project instructions).
type Pack struct {
	Spec     *afspec.Spec
	Dir      string // the spec directory, absolute
	Root     string // the repository root
	Steering string // .specs/steering.md, empty when absent or placeholder-only
	Project  string // AGENTS.md or CLAUDE.md, empty when absent
	// tasksHadSchema records whether tasks.json carried a "$schema" key when
	// it was read, so it is written back the way it was found.
	tasksHadSchema bool
}

// ResolveSpecDir turns the positional argument into a spec directory, the way
// the spec CLI's resolveSpec does: an existing directory is used as given; a
// number matches the `NN_` prefix under specsDir ("1" matches 1_foo and
// 01_foo, not 10_bar); anything else must be an exact directory name.
func ResolveSpecDir(specsDir, arg string) (string, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return filepath.Abs(arg)
	}
	entries, err := os.ReadDir(specsDir)
	if err != nil {
		return "", fmt.Errorf("spec %q is not a directory and %s cannot be read: %w", arg, specsDir, err)
	}
	var matches []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == arg {
			return filepath.Abs(filepath.Join(specsDir, name))
		}
		if n, err := strconv.Atoi(arg); err == nil {
			if prefix, _, perr := afspec.ParseSpecDirName(name); perr == nil {
				if p, err := strconv.Atoi(prefix); err == nil && p == n {
					matches = append(matches, name)
				}
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no spec matching %q under %s", arg, specsDir)
	case 1:
		return filepath.Abs(filepath.Join(specsDir, matches[0]))
	default:
		return "", fmt.Errorf("%q is ambiguous under %s: %s", arg, specsDir, strings.Join(matches, ", "))
	}
}

// LoadPack loads and validates the spec. Validation errors are fatal —
// agent-fox's planner refuses a pack afspec cannot load, and a pack whose
// tasks.json cannot be saved back is one this program cannot finish. A pack
// that depends on another spec is refused too; see LoadPackWith.
func LoadPack(root, specDir string) (*Pack, []string, error) {
	return LoadPackWith(root, specDir, false)
}

// LoadPackWith is LoadPack with the dependency check loosened.
//
// agent-fox schedules a group behind the group of the other spec it depends
// on. flatline runs one spec and cannot do that, so by default a pack with
// cross-spec dependencies is refused. With assumeDeps the dependencies are
// taken to be in place already — the other spec was implemented earlier, by
// hand or by another run — and each is reported as a warning and rendered
// into the coder's context, so the assumption is visible rather than silent.
func LoadPackWith(root, specDir string, assumeDeps bool) (*Pack, []string, error) {
	spec, hadSchema, err := loadSpecTolerant(specDir)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	res := spec.Validate()
	for _, w := range res.Warnings {
		warnings = append(warnings, warningText(w))
	}
	if !res.Valid {
		var b strings.Builder
		fmt.Fprintf(&b, "%s does not validate:", specDir)
		for _, e := range res.Errors {
			fmt.Fprintf(&b, "\n  [%s] %s: %s", e.Category, e.Artifact, e.Message)
		}
		return nil, warnings, errors.New(b.String())
	}
	switch spec.Status {
	case "sealed", "superseded", "archived":
		return nil, warnings, fmt.Errorf("spec %s is %s; its tasks.json cannot be updated", spec.SpecName, spec.Status)
	}
	if n := len(spec.Tasks.Dependencies); n > 0 {
		if !assumeDeps {
			return nil, warnings, fmt.Errorf("spec %s declares %d cross-spec dependenc%s; flatline runs the linear, "+
				"single-spec case only — pass --assume-deps if the spec%s it depends on %s already implemented, "+
				"or use agent-fox",
				spec.SpecName, n, plural(n, "y", "ies"), plural(n, "", "s"), plural(n, "is", "are"))
		}
		for _, d := range spec.Tasks.Dependencies {
			warnings = append(warnings, fmt.Sprintf("assumed in place: spec %s group %d, needed from task group %d (%s)",
				d.DependsOnSpec, d.FromGroup, d.ToGroup, d.Relationship))
		}
	}
	return &Pack{
		Spec:           spec,
		Dir:            specDir,
		Root:           root,
		Steering:       LoadSteering(root),
		Project:        loadProjectInstructions(root),
		tasksHadSchema: hadSchema["tasks.json"],
	}, warnings, nil
}

// jsonArtifacts are the three JSON files and the "$schema" URI each declares.
var jsonArtifacts = map[string]string{
	"requirements.json": "https://agent-fox.dev/schemas/requirements.v1.json",
	"test_spec.json":    "https://agent-fox.dev/schemas/test_spec.v1.json",
	"tasks.json":        "https://agent-fox.dev/schemas/tasks.v1.json",
}

// loadSpecTolerant is afspec.LoadSpec for packs written by the Python
// library.
//
// The format and the JSON Schemas say "$schema" is required, and the Go
// decoder enforces that at load time. The Python afspec — the implementation
// agent-fox runs and the one that wrote most packs in the wild — models the
// key as optional and OMITS it when unset, so a pack straight out of `spec
// generate` fails to load here with "field $schema in RequirementsV1Json:
// required". The key carries no information beyond which schema the file
// follows, and that is fixed per file name; so when it is missing the file is
// loaded as if it declared the canonical URI, from a temporary copy, and the
// original is left exactly as it was. The returned map says which files had
// it, so a write-back can match.
func loadSpecTolerant(specDir string) (*afspec.Spec, map[string]bool, error) {
	had := map[string]bool{}
	patched := map[string][]byte{}
	for name, uri := range jsonArtifacts {
		raw, err := os.ReadFile(filepath.Join(specDir, name))
		if err != nil {
			continue // LoadSpec reports missing files with its own error
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			continue // likewise for malformed JSON
		}
		if _, ok := top["$schema"]; ok {
			had[name] = true
			continue
		}
		top["$schema"] = json.RawMessage(fmt.Sprintf("%q", uri))
		b, err := json.Marshal(top)
		if err != nil {
			return nil, nil, err
		}
		patched[name] = b
	}
	if len(patched) == 0 {
		spec, err := afspec.LoadSpec(specDir)
		return spec, had, err
	}

	// The copy keeps the directory's basename: the cross-file check compares
	// it with spec_id and spec_name.
	tmp, err := os.MkdirTemp("", "flatline-load-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	copyDir := filepath.Join(tmp, filepath.Base(specDir))
	if err := os.Mkdir(copyDir, 0o755); err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(specDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, ok := patched[e.Name()]
		if !ok {
			if data, err = os.ReadFile(filepath.Join(specDir, e.Name())); err != nil {
				return nil, nil, err
			}
		}
		if err := os.WriteFile(filepath.Join(copyDir, e.Name()), data, 0o644); err != nil {
			return nil, nil, err
		}
	}
	spec, err := afspec.LoadSpec(copyDir)
	if err != nil {
		return nil, nil, err
	}
	spec.Dir = specDir
	return spec, had, nil
}

// warningText renders a validation warning with whatever context it carries.
func warningText(w afspec.ValidationEntry) string {
	var parts []string
	for _, p := range []string{w.Artifact, w.Check, w.EntityID} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return w.Message
	}
	return strings.Join(parts, " ") + ": " + w.Message
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Groups returns the task groups in execution order. The spec format numbers
// groups sequentially and agent-fox's topological sort reduces to that order
// when there are no dependencies, so this is a sort by id.
func (p *Pack) Groups() []afspec.TaskGroup {
	out := append([]afspec.TaskGroup(nil), p.Spec.Tasks.TaskGroups...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// Group returns the current in-memory copy of a group.
func (p *Pack) Group(id int) (afspec.TaskGroup, bool) {
	for _, g := range p.Spec.Tasks.TaskGroups {
		if g.Id == id {
			return g, true
		}
	}
	return afspec.TaskGroup{}, false
}

// GroupComplete is agent-fox's parser rule: every non-dropped subtask is done,
// and a group with nothing but dropped subtasks is vacuously complete.
func GroupComplete(g afspec.TaskGroup) bool {
	for _, s := range g.Subtasks {
		if s.State != afspec.SubtaskStateDone && s.State != afspec.SubtaskStateDropped {
			return false
		}
	}
	return true
}

// MarkGroupDone moves every non-dropped subtask of the group to `done`
// through the state machine, and writes tasks.json.
//
// agent-fox calls afspec's complete_subtask_states, which sets non-dropped
// subtasks to done in one step. The Go port's CompleteSubtaskStates marks
// dropped ones done as well — a divergence from the Python behaviour and from
// the format's terminal-state rule — so this walks TransitionSubtask instead:
// the library validates each step, and dropped subtasks are never touched.
func (p *Pack) MarkGroupDone(groupID int) error {
	g, ok := p.Group(groupID)
	if !ok {
		return fmt.Errorf("task group %d not in tasks.json", groupID)
	}
	tasks := p.Spec.Tasks
	for _, s := range g.Subtasks {
		next, err := transitionToDone(tasks, s)
		if err != nil {
			return err
		}
		tasks = next
	}
	p.Spec.Tasks = tasks
	return p.WriteTasks()
}

// pathToDone is the legal route from each state to done, per the format's
// state machine.
var pathToDone = map[afspec.SubtaskState][]afspec.SubtaskState{
	afspec.SubtaskStatePending:             {afspec.SubtaskStateQueued, afspec.SubtaskStateInProgress, afspec.SubtaskStateDone},
	afspec.SubtaskStateQueued:              {afspec.SubtaskStateInProgress, afspec.SubtaskStateDone},
	afspec.SubtaskStateInProgress:          {afspec.SubtaskStateDone},
	afspec.SubtaskStatePendingReevaluation: {afspec.SubtaskStatePending, afspec.SubtaskStateQueued, afspec.SubtaskStateInProgress, afspec.SubtaskStateDone},
}

func transitionToDone(tasks *afspec.TasksV1Json, s afspec.Subtask) (*afspec.TasksV1Json, error) {
	if s.State == afspec.SubtaskStateDone || s.State == afspec.SubtaskStateDropped {
		return tasks, nil
	}
	path, ok := pathToDone[s.State]
	if !ok {
		return nil, fmt.Errorf("subtask %s is in state %q, which has no route to done", s.Id, s.State)
	}
	for _, target := range path {
		next, err := tasks.TransitionSubtask(s.Id, string(target))
		if err != nil {
			return nil, err
		}
		tasks = next
	}
	return tasks, nil
}

// WriteTasks persists tasks.json alone, in the library's canonical encoding.
//
// afspec's Save would also re-render prd.md, recompute test_spec.json's
// coverage block and apply the lifecycle guards. The only thing this program
// is entitled to change is subtask state, so it writes the one file that
// carries it, with the same encoder Save uses.
func (p *Pack) WriteTasks() error {
	data, err := afspec.MarshalJSON(p.Spec.Tasks)
	if err != nil {
		return fmt.Errorf("encoding tasks.json: %w", err)
	}
	if !p.tasksHadSchema {
		// The file did not declare "$schema" when it was read (the Python
		// library omits it); a state update is not the place to add it. The
		// encoder writes the key first, on its own line, and omits it only
		// when nil — which a string field never is — so it is removed here.
		data = schemaLine.ReplaceAll(data, []byte("{\n"))
	}
	path := filepath.Join(p.Dir, "tasks.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// schemaLine matches the "$schema" property at the top of a canonical file.
var schemaLine = regexp.MustCompile(`^\{\n  "\$schema": "[^"\n]*",\n`)

// TasksRelPath is tasks.json relative to the repository root, for `git add`.
func (p *Pack) TasksRelPath() (string, error) {
	return filepath.Rel(p.Root, filepath.Join(p.Dir, "tasks.json"))
}

// ------------------------------------------------------------- context --

// maxContextTokens is agent-fox's default budget for the rendered spec.
const maxContextTokens = 30_000

// contextArtifacts is agent-fox's per-archetype artifact filter.
var contextArtifacts = map[Archetype][]string{
	Coder:    {"requirements", "test_spec", "tasks", "architecture"},
	Gate:     {"requirements", "tasks"},
	Verifier: {"requirements", "tasks"},
}

// sectionHeaders are agent-fox's, in its order.
var sectionHeaders = [][2]string{
	{"requirements", "## Requirements"},
	{"test_spec", "## Test Specification"},
	{"tasks", "## Tasks"},
}

// AssembleContext is agent-fox's assemble_context for one archetype and one
// task group: the spec artifacts rendered and scoped to the group, the
// steering directives, and the memory facts — joined by `---`.
//
// Two additions this program makes because it does not run inside a coding
// CLI: the `## Test Commands` block (the Go renderer omits what the Python
// renderer includes, and the verifier profile refers to it by name) and the
// project's own AGENTS.md / CLAUDE.md, which the Claude CLI would otherwise
// pick up from the working directory by itself.
func (p *Pack) AssembleContext(arch Archetype, group int, memoryFacts []string) string {
	rendered := p.Spec.RenderIndividualScoped(group, afspec.WithMaxTokens(maxContextTokens))
	active := map[string]bool{}
	for _, a := range contextArtifacts[arch] {
		active[a] = true
	}

	var sections []string
	for _, kv := range sectionHeaders {
		key, header := kv[0], kv[1]
		if !active[key] {
			sections = append(sections, header+"\n\n_(Omitted — not required for this session.)_")
			continue
		}
		body := strings.TrimSpace(rendered[key])
		if key == "tasks" {
			body = TestCommandsBlock(p.Spec.Tasks.TestCommands) + "\n\n" + body
			if deps := DependenciesBlock(p.Spec.Tasks.Dependencies); deps != "" {
				body = deps + "\n\n" + body
			}
		}
		if body != "" {
			// The library's tasks renderer already starts with "## Tasks".
			if strings.HasPrefix(body, header) || strings.Contains(body, "\n"+header+"\n") {
				sections = append(sections, body)
			} else {
				sections = append(sections, header+"\n\n"+body)
			}
		}
	}

	if active["architecture"] {
		if arch, ok := rendered["architecture"]; ok && strings.TrimSpace(arch) != "" {
			sections = append(sections, "## Architecture\n\n"+strings.TrimSpace(arch))
		} else if p.Spec.Architecture != "" {
			sections = append(sections, "## Architecture\n\n_(Omitted — excluded by token budget.)_")
		}
	} else {
		sections = append(sections, "## Architecture\n\n_(Omitted — not required for this session.)_")
	}

	if p.Steering != "" {
		sections = append(sections, "## Steering Directives\n\n"+p.Steering)
	}
	if p.Project != "" {
		sections = append(sections, "## Project Instructions\n\n"+p.Project)
	}
	if len(memoryFacts) > 0 {
		var b strings.Builder
		b.WriteString("## Memory Facts\n\n")
		for _, f := range memoryFacts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		sections = append(sections, strings.TrimRight(b.String(), "\n"))
	}
	if arch == Verifier {
		sections = append(sections, p.VerificationChecklist())
	}
	return strings.Join(sections, "\n\n---\n\n")
}

// TestCommandsBlock is the Python renderer's `## Test Commands` section.
func TestCommandsBlock(tc afspec.TestCommands) string {
	return fmt.Sprintf("## Test Commands\n\n- Spec tests: `%s`\n- All tests: `%s`\n- Linter: `%s`",
		tc.SpecTests, tc.AllTests, tc.Linter)
}

// DependenciesBlock is the Python renderer's `## Dependencies` table, with a
// line saying what flatline assumes about it. Empty when there are none.
func DependenciesBlock(deps []afspec.TaskDependency) string {
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Dependencies\n\n")
	b.WriteString("| Depends On | From Group | To Group | Relationship |\n|------------|-----------|----------|--------------|\n")
	for _, d := range deps {
		fmt.Fprintf(&b, "| %s | %d | %d | %s |\n", d.DependsOnSpec, d.FromGroup, d.ToGroup, strings.ReplaceAll(d.Relationship, "|", "\\|"))
	}
	b.WriteString("\nThese dependencies are assumed to be in place: the specs above were implemented before this run. " +
		"If what they were expected to provide is missing, say so in your summary rather than re-implementing it.")
	return b.String()
}

// VerificationChecklist is the verifier's requirement-to-test coverage table,
// computed from the pack rather than asserted.
func (p *Pack) VerificationChecklist() string {
	var b strings.Builder
	b.WriteString("## Verification Checklist\n\n### Requirement-to-Test Coverage\n\n")
	b.WriteString("| Requirement | Coverage | Tests |\n|---|---|---|\n")
	cov := p.Spec.TestSpec.ComputeCoverage(p.Spec.Requirements)
	tests := map[string][]string{}
	for _, t := range p.Spec.TestSpec.TestCases {
		tests[t.RequirementId] = append(tests[t.RequirementId], t.Id)
	}
	for _, t := range p.Spec.TestSpec.EdgeCaseTests {
		tests[t.RequirementId] = append(tests[t.RequirementId], t.Id)
	}
	for _, id := range cov.Covered {
		fmt.Fprintf(&b, "| %s | covered | %s |\n", id, strings.Join(tests[id], ", "))
	}
	for _, id := range cov.Uncovered {
		fmt.Fprintf(&b, "| %s | **UNCOVERED** | — |\n", id)
	}
	b.WriteString("\n### Subtasks\n\n")
	for _, g := range p.Groups() {
		for _, s := range g.Subtasks {
			box := "[ ]"
			if s.State == afspec.SubtaskStateDone {
				box = "[x]"
			}
			fmt.Fprintf(&b, "- %s %s %s (%s)\n", box, s.Id, s.Title, s.State)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ------------------------------------------------------------ steering --

const steeringPlaceholder = "<!-- steering:placeholder -->"

// LoadSteering reads `{root}/.specs/steering.md`, the path agent-fox hardcodes
// regardless of where the specs live. A symlink is refused, and a file that
// holds only the placeholder marker and HTML comments is treated as absent.
func LoadSteering(root string) string {
	path := filepath.Join(root, ".specs", "steering.md")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(b)
	if strings.Contains(text, steeringPlaceholder) && onlyComments(text) {
		return ""
	}
	return strings.TrimSpace(text)
}

// onlyComments reports whether the text is nothing but HTML comments and
// whitespace.
func onlyComments(s string) bool {
	for {
		s = strings.TrimSpace(s)
		if s == "" {
			return true
		}
		if !strings.HasPrefix(s, "<!--") {
			return false
		}
		end := strings.Index(s, "-->")
		if end < 0 {
			return false
		}
		s = s[end+3:]
	}
}

// loadProjectInstructions returns AGENTS.md, else CLAUDE.md, from the root.
func loadProjectInstructions(root string) string {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// SubtaskDescriptions is agent-fox's extract_subtask_descriptions: the first
// detail bullet of each subtask, used as the one-line task description.
func SubtaskDescriptions(g afspec.TaskGroup) []string {
	var out []string
	for _, s := range g.Subtasks {
		if len(s.Details) > 0 && !strings.HasPrefix(strings.TrimSpace(s.Details[0]), "_") {
			out = append(out, s.Details[0])
		} else {
			out = append(out, s.Title)
		}
	}
	return out
}
