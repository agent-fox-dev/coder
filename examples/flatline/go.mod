// flatline is a SEPARATE MODULE, like difftest/.
//
// It imports the agent-fox spec library (github.com/agent-fox-dev/spec), which
// brings a YAML parser and a JSON Schema validator with it. REQ-GO-11 holds the
// root module to the standard library, and a nested module is the only
// mechanism in Go that keeps a dependency out of the root's graph.
//
// The spec library's go.mod lives in the golang/ subdirectory of its repository
// but declares the repository-root module path, so the module proxy serves an
// empty module for it and `go get` cannot fetch the code. Until that is fixed
// upstream it is consumed through a replace to a sibling checkout:
//
//	git clone https://github.com/agent-fox-dev/spec ../../../spec
//
// or point the replace elsewhere:
//
//	go mod edit -replace github.com/agent-fox-dev/spec=/path/to/spec/golang
module github.com/agentfox/agentkit-go/examples/flatline

go 1.26.5

require (
	github.com/agent-fox-dev/spec v1.4.1
	github.com/agentfox/agentkit-go v0.0.0
)

require (
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.14.0 // indirect
)

replace github.com/agentfox/agentkit-go => ../..

replace github.com/agent-fox-dev/spec => ../../../spec/golang
