package main

import "testing"

// TestCandidateLogPaging exercises the ring buffer past wraparound and checks
// pagination ordering (newest first), page boundaries, and lifetime totals.
func TestCandidateLogPaging(t *testing.T) {
	l := NewCandidateLog()

	// Empty log
	events, total := l.Page(1, 25)
	if len(events) != 0 || total != 0 {
		t.Fatalf("empty log: got %d events, total %d", len(events), total)
	}

	// Add 300 events (wraps the 250-entry ring); alternate reasons.
	for i := 1; i <= 300; i++ {
		reason := reasonTooShort
		if i%10 == 0 {
			reason = reasonAccepted
		}
		l.Add(CandidateEvent{TimestampNs: int64(i), Reason: reason, Accepted: reason == reasonAccepted})
	}

	// Ring holds the newest 250 (seq 51..300)
	if _, total = l.Page(1, 25); total != candidateLogDepth {
		t.Fatalf("total = %d, want %d", total, candidateLogDepth)
	}

	// Page 1: newest first → seq 300, 299, ...
	events, _ = l.Page(1, 25)
	if len(events) != 25 || events[0].Seq != 300 || events[24].Seq != 276 {
		t.Fatalf("page 1: len=%d first=%d last=%d", len(events), events[0].Seq, events[len(events)-1].Seq)
	}

	// Page 2 continues where page 1 ended
	events, _ = l.Page(2, 25)
	if events[0].Seq != 275 {
		t.Fatalf("page 2 first seq = %d, want 275", events[0].Seq)
	}

	// Last page is a partial page ending at the oldest retained entry (seq 51)
	events, _ = l.Page(10, 25)
	if len(events) != 25 || events[24].Seq != 51 {
		t.Fatalf("page 10: len=%d last=%d, want 25 & 51", len(events), events[24].Seq)
	}

	// Beyond the end → empty
	if events, _ = l.Page(11, 25); len(events) != 0 {
		t.Fatalf("page 11: got %d events, want 0", len(events))
	}

	// Totals are lifetime counts (all 300, not just the retained 250)
	totals := l.Totals()
	if totals[reasonAccepted] != 30 || totals[reasonTooShort] != 270 {
		t.Fatalf("totals = %v, want accepted=30 too_short=270", totals)
	}
}
