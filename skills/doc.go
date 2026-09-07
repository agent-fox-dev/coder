// Package skills implements the skills system (PRD §6.5), project context
// files (§6.5a) and the project trust gate (REQ-SKILL-12, REQ-CTX-03,
// REQ-SEC-10).
//
// The package has one job that matters more than the rest: everything it
// discovers under the working directory is UNTRUSTED INPUT. A skill's name and
// description are authored into the system prompt together with an instruction
// to read its file, and a context file's entire body is. A hostile repository
// therefore authors part of the system prompt just by being the current
// directory — git clone, cd, run. Config.TrustProject is a bool whose zero
// value is false, so an embedder that says nothing gets nothing (REQ-SKILL-12).
//
// The other half of the design is progressive disclosure (REQ-SKILL-06). A
// skill contributes exactly three things to the prompt — name, description and
// the absolute path to its prompt file — so the cost of offering N skills is
// N lines regardless of how large they are, and the model pays for a skill's
// body only when it decides the skill applies and reads the file itself.
//
// # The prompt authoring contract (REQ-SKILL-13)
//
// §6.5 specifies the container — skill.toml, prompt.md, the directory. What
// goes INSIDE prompt.md is a contract of its own, and the built-in skills
// under `_skills/` (see BuiltinDir) are its worked examples. A conforming
// prompt has six parts, in this order, each under its own heading:
//
//  1. Charter. What the skill is responsible for and, explicitly, what it is
//     NOT. Without a stated non-goal, two skills loaded into one session both
//     do the easy half of a job and neither does the hard half.
//  2. Hard prohibitions, each recorded WITH THE INCIDENT that produced it. A
//     rule with no stated cause reads as boilerplate and is reasoned away by
//     the next model that encounters it.
//  3. Mechanical gates: commands whose exit status is a binary result
//     independent of model judgement, wherever the domain admits any.
//  4. A fixed output contract: the exact shape the skill's output takes, so a
//     caller can parse it.
//  5. Anti-false-positive carve-outs: the patterns that look like violations
//     and are deliberate, named explicitly.
//  6. Pointers, not copies. The prompt references the authoritative source
//     rather than restating it; a duplicated table drifts and then
//     contradicts its original silently.
//
// The contract is prose, not a schema: this package validates the container
// (manifest, prompt file, symlink rules) and never parses the prompt body,
// because the body's consumer is the model and a lint over it would be a
// second, weaker reader of the same text. Conformance is reviewed, and the
// built-in skill is the review's reference.
//
// What this package does NOT do, stated so nobody reports it as done:
//
//   - It does not load Go plugin code, so REQ-SKILL-09's import lint and the
//     [skill.tools] module/factory pair are parsed and carried, not executed.
//     MergeTools takes the tool values from the embedder.
//   - It does not spawn subagents (REQ-SKILL-08); [skill.subagent] is parsed
//     and carried for the session runner.
//   - It does not emit the audit event of REQ-SKILL-11; Registry.Names gives
//     the caller the list to record.
//   - It does not mark tools for REQ-CACHE-10.
package skills
