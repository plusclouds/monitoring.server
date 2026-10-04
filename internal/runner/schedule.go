package runner

import (
	"container/heap"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
)

// phase is a check's fixed offset within its interval (F04): a 60 s check
// always runs at the same second of the minute, on any node and after any
// restart, which spreads load evenly and keeps graphs regular.
func phase(id uuid.UUID, interval time.Duration) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write(id[:])
	ms := interval.Milliseconds()
	if ms <= 0 {
		return 0
	}
	return time.Duration(h.Sum64()%uint64(ms)) * time.Millisecond //nolint:gosec // ms > 0
}

// nextRun is the first run time strictly after t for a check with this
// interval and phase.
func nextRun(t time.Time, interval, ph time.Duration) time.Time {
	iv, p := interval.Milliseconds(), ph.Milliseconds()
	if iv <= 0 {
		return t.Add(time.Second)
	}
	ms := t.UnixMilli() - p
	k := ms / iv
	if ms < 0 && ms%iv != 0 {
		k--
	}
	return time.UnixMilli((k+1)*iv + p)
}

// queue is a min-heap of entries by next run time.
type queue []*entry

func (q queue) Len() int           { return len(q) }
func (q queue) Less(i, j int) bool { return q[i].next.Before(q[j].next) }
func (q queue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}

func (q *queue) Push(x any) {
	e := x.(*entry)
	e.index = len(*q)
	*q = append(*q, e)
}

func (q *queue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*q = old[:n-1]
	return e
}

var _ heap.Interface = (*queue)(nil)
