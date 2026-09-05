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
