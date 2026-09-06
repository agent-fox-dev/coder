package core

// Span and Tracer live in core rather than in the root package because
// AgentConfig has to hold one: REQ-OBS-02 puts a span around every TOOL CALL,
// which happens in the batch executor, and a tracer reachable only through
// Axis 1 middleware can never see one.
//
// core holds declarations and interface seams; this is one.

type Span interface {
	SetAttributes(kv map[string]any)
	SetStatus(err error)
	AddEvent(name string, kv map[string]any)
	End()
}

// Tracer starts spans. StartSpan is callback-scoped and deliberately does NOT
// take a context.Context: cancellation belongs to the work the callback closes
// over, not to the tracing of it.
type Tracer interface {
	StartSpan(name string, fn func(Span) error) error
}

type noopSpan struct{}

func (noopSpan) SetAttributes(map[string]any)    {}
func (noopSpan) SetStatus(error)                 {}
func (noopSpan) AddEvent(string, map[string]any) {}
func (noopSpan) End()                            {}

type noopTracer struct{}

func (noopTracer) StartSpan(_ string, fn func(Span) error) error { return fn(noopSpan{}) }

// NoopTracer is the shared, fieldless default. An untraced run neither
// inspects nor retains what it is handed.
var NoopTracer Tracer = noopTracer{}
