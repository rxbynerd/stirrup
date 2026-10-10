package types

import (
	"errors"
	"fmt"
	"testing"
)

func TestMarkContextOverflow(t *testing.T) {
	base := errors.New("prompt is too long")
	marked := MarkContextOverflow(base)

	if !IsContextOverflow(marked) {
		t.Fatal("IsContextOverflow(marked) = false, want true")
	}
	if marked.Error() != base.Error() {
		t.Errorf("marked message = %q, want %q", marked.Error(), base.Error())
	}
	if !errors.Is(marked, base) {
		t.Error("marked error does not wrap the original")
	}
	if !IsContextOverflow(fmt.Errorf("execute request: %w", marked)) {
		t.Error("IsContextOverflow does not see a mark under a wrapping error")
	}
	if IsContextOverflow(base) {
		t.Error("IsContextOverflow(unmarked) = true, want false")
	}
	if IsContextOverflow(nil) {
		t.Error("IsContextOverflow(nil) = true, want false")
	}
	if MarkContextOverflow(nil) != nil {
		t.Error("MarkContextOverflow(nil) is not nil")
	}
}
