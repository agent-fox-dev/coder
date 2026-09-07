// Package testingexample is an example in the form of a test file.
//
// It answers the question every consumer hits once their agent works:
// how do I test it? The code under test is not this SDK — it is the tool
// handlers, interceptors and middleware YOU write on top of it, and all of
// them only really run inside the loop. Testing them by calling a real model
// is slow, costs money and is not deterministic; testing them by hand-rolling
// a fake provider means guessing at the loop's contract.
//
// So the SDK ships one: provider/faux. It is exported, supported API sitting
// next to the real providers, it replays a scripted sequence of assistant
// turns, and the event order it emits is the normative one. Scripting a turn
// is the whole setup.
//
// Read examples/testing/agent_test.go top to bottom. Each test function is one
// technique, in the order you will want them:
//
//  1. Script a multi-turn run and assert what came out.
//  2. Assert what was SENT — the system prompt, the tool declarations and the
//     tool result in the transcript. faux.Provider.Requests is the only way to
//     see this.
//  3. Test a tool handler with no agent at all. The cheapest test you can
//     write, and where most tool bugs actually are.
//  4. Test a BeforeToolCall interceptor: a blocked call must produce an error
//     result AND leave the run alive.
//  5. Test custom middleware, including the ordering rule that trips people
//     up: the LAST registered middleware is the OUTERMOST.
//  6. Assert the event sequence a streaming UI is written against.
//  7. Assert the failure paths deterministically — no sleeps. A turn that
//     started always ends with a terminal message, and an aborted run still
//     leaves a transcript you can resume.
//
// Run it:
//
//	go test ./examples/testing/ -v
//
// There is no API key, no network access and no environment variable in any of
// it. That is the point: these are tests you can run in CI on every commit.
// Copy the file into your own repository, swap the tools for yours, and delete
// what you do not need.
package testingexample
