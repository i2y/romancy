package storage

import (
	"testing"
	"time"
)

func TestRoundUpDeadline(t *testing.T) {
	base := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct{ in, want time.Time }{
		{base, base},
		{base.Add(time.Nanosecond), base.Add(time.Second)},
		{base.Add(300 * time.Millisecond), base.Add(time.Second)},
		{base.Add(999 * time.Millisecond), base.Add(time.Second)},
		{base.Add(time.Second), base.Add(time.Second)},
	} {
		if got := RoundUpDeadline(c.in); !got.Equal(c.want) {
			t.Errorf("RoundUpDeadline(%v) = %v; want %v", c.in, got, c.want)
		}
	}
}
