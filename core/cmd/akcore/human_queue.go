package main

import (
	"fmt"
	"sync"

	"agentkate/internal/agent"
)

// humanSendQueue is the core-owned follow-up queue shared by human surfaces.
// A remote device may only enqueue while an agent is busy; it never writes a
// second prompt into a live turn. The queue contains already-authorised human
// sends, not a principal or an IPC request, so it cannot become a way to
// replay authority after its session has been revoked.
type humanSendQueue struct {
	mu       sync.Mutex
	pending  map[string][]queuedHumanSend
	resuming map[string]bool
}

type queuedHumanSend struct {
	text        string
	attachments []agent.Attachment
}

const maxQueuedHumanSendsPerThread = 32

func newHumanSendQueue() *humanSendQueue {
	return &humanSendQueue{pending: make(map[string][]queuedHumanSend), resuming: make(map[string]bool)}
}

// beginResume atomically elects the one goroutine allowed to wake a dormant
// thread. Other phones may still add their ordered follow-up, but a double tap
// (or two paired devices) must never spawn two processes for one thread id.
func (q *humanSendQueue) beginResume(threadID string) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.resuming[threadID] {
		return false
	}
	q.resuming[threadID] = true
	return true
}

func (q *humanSendQueue) finishResume(threadID string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	delete(q.resuming, threadID)
	q.mu.Unlock()
}

func (q *humanSendQueue) enqueue(threadID, text string, atts []agent.Attachment) (int, error) {
	if q == nil {
		return 0, fmt.Errorf("human send queue is unavailable")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.pending[threadID]
	if len(items) >= maxQueuedHumanSendsPerThread {
		return 0, fmt.Errorf("the follow-up queue is full")
	}
	copyAtts := append([]agent.Attachment(nil), atts...)
	q.pending[threadID] = append(items, queuedHumanSend{text: text, attachments: copyAtts})
	return len(q.pending[threadID]), nil
}

func (q *humanSendQueue) take(threadID string) (queuedHumanSend, bool) {
	if q == nil {
		return queuedHumanSend{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.pending[threadID]
	if len(items) == 0 {
		return queuedHumanSend{}, false
	}
	item := items[0]
	if len(items) == 1 {
		delete(q.pending, threadID)
	} else {
		q.pending[threadID] = items[1:]
	}
	return item, true
}

// drainOne starts exactly one queued follow-up after a busy->idle edge. The
// delivery itself re-marks the turn busy before writing to the harness, so a
// second queue entry cannot overtake it. It returns true when it consumed an
// entry, even when that entry failed: the next terminal edge will handle the
// next entry instead of creating a write storm on a dying harness.
func (q *humanSendQueue) drainOne(d handlerDeps, threadID string) bool {
	item, ok := q.take(threadID)
	if !ok {
		return false
	}
	if err := d.deliverAcceptedHumanSend(threadID, item.text, item.attachments, false); err != nil {
		if d.log != nil {
			d.log.Warn("queued human send was not delivered", "thread", threadID, "err", err)
		}
	}
	return true
}
