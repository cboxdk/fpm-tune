package plan

import (
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
)

// TestAPoolThatWillNotBeWrittenDoesNotShowAPlan.
//
// The PLAN column is a promise: those are the numbers that will be in the pool
// directory after `apply`. A pool whose current configuration could not be read
// is accounted for in the budget and deliberately not written — setting a
// ceiling means replacing a known one — so printing a number for it in that
// column sends an operator away expecting a change that will not arrive.
func TestAPoolThatWillNotBeWrittenDoesNotShowAPlan(t *testing.T) {
	var b strings.Builder
	err := Result{
		Budget: budget.Limits{MemoryBytes: 4 * gb, Source: budget.SourceMemInfo},
		Plan: allocate.Plan{
			TotalBytes: 4 * gb,
			Pools: []allocate.PoolPlan{
				{Name: "readable", MaxChildren: 12, Current: 8, Bytes: 512 * mb},
				{Name: "unreadable", MaxChildren: 20, Current: 20, Bytes: 900 * mb, Unknown: true},
			},
		},
		Unreachable: []string{"unreadable"},
	}.Render(&b)
	if err != nil {
		t.Fatal(err)
	}

	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.HasPrefix(line, "unreadable") {
			continue
		}
		if strings.Contains(line, "20") && !strings.Contains(line, "—") {
			t.Errorf("a pool that will not be written shows a plan number:\n%s", line)
		}

		return
	}
	t.Fatalf("the pool is missing from the table entirely:\n%s", b.String())
}
