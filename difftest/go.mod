// The differential harness is a SEPARATE MODULE (NFR-TEST-06.7).
//
// The reference bodies it compares against are produced by an independent
// implementation — a vendor SDK at a pinned version, or recorded live traffic.
// Whatever that costs in dependencies must not land in the graph of anyone who
// imports AgentKit (REQ-GO-11), and the only way to guarantee that is a module
// boundary rather than a build tag.
module github.com/agentfox/agentkit-go/difftest

go 1.24

require github.com/agentfox/agentkit-go v0.0.0

replace github.com/agentfox/agentkit-go => ..
