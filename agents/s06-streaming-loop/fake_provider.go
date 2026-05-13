package main

import (
	"context"
	"errors"
	"io"
)

// fakeProvider replays a scripted slice of Events through the Stream
// interface — the testing analogue of s05's httptest.NewServer. Tests
// construct one with the exact Events they want the Loop to see, exercise
// Loop.Consume against it, and assert on the resulting Message.
//
// Why a fake instead of going back to httptest + canned SSE: s06's unit
// under test is the Loop, not the Anthropic SSE parser (s05 already
// covered that). Decoupling the two means a Loop test failure points at
// the assembly logic, not at byte-level SSE quirks.
type fakeProvider struct {
	// events is the script the next Stream returns from Next() in order.
	events []Event

	// errAt, if non-zero, is the 1-indexed position at which Next() returns
	// errAtError instead of the scripted Event. Lets a test inject a
	// mid-stream failure (e.g. simulated network drop) without having to
	// build a separate fake.
	errAt      int
	errAtError error

	// blockOn, if non-nil, is a channel Stream.Next() blocks on (after
	// emitting `unblockAfter` Events) until either the channel sends or
	// ctx is canceled. This is the hook the abort test uses to guarantee
	// the Loop is actually mid-stream when Cancel() fires.
	blockOn       <-chan struct{}
	unblockAfter  int

	// streamErr, if non-nil, is what Stream() itself returns instead of
	// constructing a stream. Used to test the "provider failed before
	// the stream began" path.
	streamErr error
}

// Stream returns a *fakeStream backed by p.events. The req argument is
// ignored — the fake doesn't validate the request shape; that's s05's
// concern.
func (p *fakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	return &fakeStream{
		ctx:          ctx,
		events:       p.events,
		errAt:        p.errAt,
		errAtError:   p.errAtError,
		blockOn:      p.blockOn,
		unblockAfter: p.unblockAfter,
	}, nil
}

// fakeStream is the *Stream the fakeProvider returns. It walks `events`
// linearly, returning io.EOF after the last one — same end-of-stream
// signal real implementations use.
type fakeStream struct {
	ctx          context.Context
	events       []Event
	idx          int
	errAt        int
	errAtError   error
	blockOn      <-chan struct{}
	unblockAfter int
	closed       bool
}

// Next returns the next scripted Event, or io.EOF when the script is
// exhausted. If the test asked for a mid-stream error or block, that
// happens on the configured iteration.
func (s *fakeStream) Next() (Event, error) {
	if s.closed {
		// A closed stream returning io.EOF mirrors real impls — the Loop
		// shouldn't be calling Next() after Close() anyway, but defensive
		// programming costs nothing here.
		return Event{}, io.EOF
	}

	// 1-indexed position counter — the test API asks "fail at the Nth
	// Next() call," which is more readable than zero-based indexing.
	pos := s.idx + 1

	// Scripted error injection: fire BEFORE emitting the scripted event
	// at this position, so a test can fail-at-2 to mean "first event
	// succeeds, second call returns error."
	if s.errAt > 0 && pos == s.errAt && s.errAtError != nil {
		return Event{}, s.errAtError
	}

	// Scripted block: after emitting unblockAfter events, hold here until
	// the test sends on blockOn or ctx is canceled. This is the Loop
	// abort test's hook — it cancels ctx, the block returns ctx.Err(),
	// the Loop sees the error and propagates context.Canceled to the
	// caller.
	if s.blockOn != nil && s.idx >= s.unblockAfter {
		select {
		case <-s.blockOn:
		case <-s.ctx.Done():
			return Event{}, s.ctx.Err()
		}
	}

	if s.idx >= len(s.events) {
		return Event{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

// Close marks the stream closed. Idempotent — same contract as s05.
func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

// Compile-time check that *fakeProvider satisfies Provider. If the
// Provider interface ever grows a method, this line breaks the build at
// the fake's site, not deep inside a test failure.
var _ Provider = (*fakeProvider)(nil)

// errAbortRequested is the canonical "test asked us to fail" sentinel for
// fakes that don't care about the specific error shape. Tests that DO care
// (e.g. the malformed-event test) construct their own.
var errAbortRequested = errors.New("fake: scripted abort")
