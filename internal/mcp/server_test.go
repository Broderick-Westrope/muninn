package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestTextHandlerTurnsPanicIntoToolError(t *testing.T) {
	h := textHandler(func(context.Context, struct{}) (string, error) {
		panic("boom")
	})

	_, _, err := h(context.Background(), nil, struct{}{})

	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want an error naming the panic", err)
	}
}
