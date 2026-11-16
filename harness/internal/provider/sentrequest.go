package provider

import (
	"context"
	"sync"
)

// SentRequest collects what adapters report about the request they put on
// the wire, so telemetry records the parameters actually sent rather than
// those the caller asked for.
type SentRequest struct {
	mu          sync.Mutex
	temperature *float64
}

type sentRequestKey struct{}

// WithSentRequest returns a context whose adapter calls report into the
// returned SentRequest.
func WithSentRequest(ctx context.Context) (context.Context, *SentRequest) {
	s := &SentRequest{}
	return context.WithValue(ctx, sentRequestKey{}, s), s
}

// Temperature returns the sampling temperature the most recent request
// carried, or nil when it carried none or the adapter reported nothing.
func (s *SentRequest) Temperature() *float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.temperature == nil {
		return nil
	}
	t := *s.temperature
	return &t
}

// recordSentTemperature reports the temperature a request body carries;
// nil means the body carries none.
func recordSentTemperature(ctx context.Context, temperature *float64) {
	s, ok := ctx.Value(sentRequestKey{}).(*SentRequest)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.temperature = nil
	if temperature != nil {
		t := *temperature
		s.temperature = &t
	}
}
