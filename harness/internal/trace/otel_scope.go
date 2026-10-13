package trace

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// sessionIDKey is the general semconv session attribute. Some backends
// group spans into sessions by it rather than by gen_ai.conversation.id,
// so both carry the session name.
const sessionIDKey = "session.id"

const (
	agentNameRun      = "stirrup"
	agentNameSubAgent = "stirrup.subagent"
)

// runScopeProcessor stamps run-wide GenAI identity onto every span at
// start: the session name as gen_ai.conversation.id and session.id, and
// gen_ai.agent.name inherited from the nearest enclosing invoke_agent
// span. Lineage is tracked by span ID rather than a context value
// because the loop parents tool and chat spans on contexts derived from
// the cancellable run context, which never carried the agent span's
// values.
//
// The mutex is a leaf: the processor never calls into the emitter.
type runScopeProcessor struct {
	mu             sync.Mutex
	conversationID string
	agents         map[oteltrace.SpanID]string
}

var _ sdktrace.SpanProcessor = (*runScopeProcessor)(nil)

func newRunScopeProcessor() *runScopeProcessor {
	return &runScopeProcessor{agents: map[oteltrace.SpanID]string{}}
}

// setRun records the conversation ID for the run being started; empty
// means no session name was configured.
func (p *runScopeProcessor) setRun(conversationID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conversationID = conversationID
}

func (p *runScopeProcessor) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	own := ""
	for _, kv := range s.Attributes() {
		if kv.Key == genAIAgentNameKey {
			own = kv.Value.AsString()
			break
		}
	}

	p.mu.Lock()
	agent := own
	if agent == "" {
		agent = p.agents[s.Parent().SpanID()]
	}
	if agent == "" {
		agent = agentNameRun
	}
	p.agents[s.SpanContext().SpanID()] = agent
	conversationID := p.conversationID
	p.mu.Unlock()

	attrs := make([]attribute.KeyValue, 0, 3)
	if own == "" {
		attrs = append(attrs, attribute.String(genAIAgentNameKey, agent))
	}
	if conversationID != "" {
		attrs = append(attrs,
			attribute.String(genAIConversationIDKey, conversationID),
			attribute.String(sessionIDKey, conversationID),
		)
	}
	if len(attrs) > 0 {
		s.SetAttributes(attrs...)
	}
}

func (p *runScopeProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.agents, s.SpanContext().SpanID())
}

func (p *runScopeProcessor) Shutdown(context.Context) error { return nil }

func (p *runScopeProcessor) ForceFlush(context.Context) error { return nil }

// OTelOption configures an OTelTraceEmitter at construction.
type OTelOption func(*otelOptions)

type otelOptions struct {
	spanProcessors []sdktrace.SpanProcessor
}

// WithSpanProcessor registers an extra span processor after the emitter's
// run-scope processor, so it sees gen_ai.agent.name, gen_ai.conversation.id
// and session.id already set at OnStart. A processor can only change a
// span at start; end-time values reach it read-only.
func WithSpanProcessor(p sdktrace.SpanProcessor) OTelOption {
	return func(o *otelOptions) {
		o.spanProcessors = append(o.spanProcessors, p)
	}
}
