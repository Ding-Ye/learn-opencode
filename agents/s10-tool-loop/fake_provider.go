package main

import (
	"context"
	"errors"
	"io"
)

// fakeProvider replays a SLICE OF EVENT SLICES — one slice per Stream call,
// indexed by call count. This is the s10 generalization of s06's
// fakeProvider (which scripted ONE stream): the tool loop calls
// Provider.Stream once per iteration, so a 3-iteration test (assistant →
// tool → assistant → tool → assistant end_turn) needs THREE scripted
// streams, indexed in order.
//
// Why an index-counter and not a queue: a queue would consume each script
// off the front, which makes "ran out of scripts" a SILENT empty-stream
// case (Next() returns EOF immediately, the loop sees no tool_use and
// terminates as if end_turn). An out-of-bounds index returns
// errOutOfScripts so a test that calls Stream more times than expected
// fails LOUDLY at the over-call site, not in the assertion that follows.
type fakeProvider struct {
	// scripts[i] is the slice of Events the i-th Stream call returns
	// from Next() in order. Indexed by callCount, not popped — so a test
	// that wants to inspect the index of the failure can.
	scripts [][]Event

	// callCount tracks how many times Stream() has been called. Bumped
	// at the START of Stream so an over-call fails the (callCount-1)
	// position with a clear "ran out of scripts" message.
	callCount int

	// streamErrAt, if non-zero, is the 1-indexed Stream call at which
	// Stream() itself returns streamErr instead of constructing a
	// stream. Lets the "Stream call N failed" path be tested without
	// rewiring the script slice.
	streamErrAt int
	streamErr   error
}

// Stream returns a *fakeStream backed by the next script. Bumps callCount
// regardless of success, so over-calling is observable.
func (p *fakeProvider) Stream(_ context.Context, _ Request) (Stream, error) {
	p.callCount++
	if p.streamErrAt > 0 && p.callCount == p.streamErrAt && p.streamErr != nil {
		return nil, p.streamErr
	}
	if p.callCount > len(p.scripts) {
		return nil, errOutOfScripts
	}
	return &fakeStream{events: p.scripts[p.callCount-1]}, nil
}

// fakeStream walks one scripted slice linearly, returning io.EOF after
// the last event — same end-of-stream signal real impls use.
type fakeStream struct {
	events []Event
	idx    int
	closed bool
}

func (s *fakeStream) Next() (Event, error) {
	if s.closed || s.idx >= len(s.events) {
		return Event{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

// Compile-time check that *fakeProvider satisfies Provider.
var _ Provider = (*fakeProvider)(nil)

// errOutOfScripts is what fakeProvider.Stream returns if the test calls
// Stream more times than there are scripts. The MaxIterations test relies
// on this NEVER firing (the loop should exit at the cap, not from script
// exhaustion); other tests rely on it as a guard against accidental
// over-call.
var errOutOfScripts = errors.New("fake: Stream called more times than scripts provided")
