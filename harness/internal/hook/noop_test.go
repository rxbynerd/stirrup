package hook

import (
	"context"
	"testing"
)

// TestNoop_ImplementsRunner is a compile-time satisfaction guard.
func TestNoop_ImplementsRunner(t *testing.T) {
	var _ Runner = NewNoop()
}

func TestNoop_RunPreAndRunPostAreNoOps(t *testing.T) {
	n := NewNoop()

	preResults, preErr := n.RunPre(context.Background())
	if preErr != nil || preResults != nil {
		t.Errorf("RunPre() = (%v, %v), want (nil, nil)", preResults, preErr)
	}

	postResults, postErr := n.RunPost(context.Background(), "success")
	if postErr != nil || postResults != nil {
		t.Errorf("RunPost() = (%v, %v), want (nil, nil)", postResults, postErr)
	}
}
