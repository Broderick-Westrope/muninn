package cli

import (
	"math"
	"runtime/debug"
	"testing"
)

func TestApplyMemoryLimit(t *testing.T) {
	tests := map[string]struct {
		env  string
		want int64
	}{
		"defaults when GOMEMLIMIT is unset":  {env: "", want: mcpMemoryLimit},
		"leaves an explicit GOMEMLIMIT alone": {env: "1GiB", want: math.MaxInt64},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			prev := debug.SetMemoryLimit(math.MaxInt64)
			t.Cleanup(func() { debug.SetMemoryLimit(prev) })

			applyMemoryLimit(tc.env)

			if got := debug.SetMemoryLimit(-1); got != tc.want {
				t.Errorf("memory limit = %d, want %d", got, tc.want)
			}
		})
	}
}
