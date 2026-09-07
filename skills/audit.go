package skills

// REQ-SKILL-11 / REQ-OBS-04: every loaded skill name is recorded in the
// session's audit event.
//
// This package cannot emit the event — the audit sink lives on the agent, and
// a skills package that imported the root agent package would invert the
// dependency REQ-SKILL-08 draws. What it can do is remove the step where an
// embedder forgets: the names come back from the same call that selects the
// skills, in the shape the sink takes, so recording them is a wrapper rather
// than a second call to remember somewhere else.

// Auditor is the audit sink, structurally satisfied by *agentkit.Agent's
// AuditSkills method. It is an interface HERE rather than a concrete type
// there so the dependency points the right way: the agent knows nothing about
// this package's selection, and this package knows nothing about the agent.
type Auditor interface {
	AuditSkills(names []string)
}

// AuditNames returns the names of a selected skill set, in selection order, as
// the audit event records them.
//
// Order is the selection's, not sorted again: LoadForSession already returns
// skills by name, and re-sorting a caller's own list would make the audit
// record disagree with the order the skills were injected in.
func AuditNames(sel []Skill) []string {
	out := make([]string, 0, len(sel))
	for _, s := range sel {
		out = append(out, s.Name)
	}
	return out
}

// Record hands a selection to the audit sink and returns it unchanged, so the
// audit is a WRAPPER around the selection instead of a separate call:
//
//	sel := skills.Record(agent, reg.LoadForSession(archetype, task, reg.Config()))
//	prompt += skills.Assemble(skills.Input{Skills: sel, Tools: active})
//
// Written that way there is no arrangement of the code in which the skills are
// injected and the audit event is not emitted, which is what REQ-SKILL-11
// actually asks for; a bare AuditSkills(names) call elsewhere in the function
// is one refactor away from being dropped.
//
// A nil Auditor is a working no-op, so a host with no audit sink configured
// uses the same line.
func Record(a Auditor, sel []Skill) []Skill {
	if a != nil {
		a.AuditSkills(AuditNames(sel))
	}
	return sel
}
