// Package audio fans a call's incoming audio frames out to any number of
// independent readers (one per plugin) and to an optional sink such as a file.
package audio

import (
	"context"
	"errors"
	"io"
	"sync"
)

// ErrClosed is returned by Publish after Close has been called.
var ErrClosed = errors.New("audio: broadcaster closed")

// Broadcaster distributes frames to subscribers. Each subscriber has its own
// unbounded queue so a slow reader never blocks ingestion or other readers.
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[*Stream]struct{}
	sink   io.Writer
	closed bool
	frames int64
	bytes  int64
}

// NewBroadcaster creates a broadcaster. sink may be nil; if set, every frame is
// written to it synchronously before being fanned out.
func NewBroadcaster(sink io.Writer) *Broadcaster {
	return &Broadcaster{subs: make(map[*Stream]struct{}), sink: sink}
}

// Subscribe returns a new Stream that receives every frame published after this
// call. Subscribe before publishing to guarantee a reader sees the whole call.
func (b *Broadcaster) Subscribe() *Stream {
	s := &Stream{b: b}
	s.cond = sync.NewCond(&s.mu)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		s.eof = true
		return s
	}
	b.subs[s] = struct{}{}
	return s
}

// Publish delivers a frame to the sink and to all subscribers. The frame is
// copied, so the caller may reuse its buffer.
func (b *Broadcaster) Publish(frame []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if b.sink != nil {
		if _, err := b.sink.Write(frame); err != nil {
			return err
		}
	}
	b.frames++
	b.bytes += int64(len(frame))
	buf := make([]byte, len(frame))
	copy(buf, frame)
	for s := range b.subs {
		s.push(buf)
	}
	return nil
}

// Close ends the stream; subscribers receive io.EOF once their queues drain.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		s.finish()
	}
	b.subs = nil
}

// Stats returns the number of frames and bytes published so far.
func (b *Broadcaster) Stats() (frames, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.frames, b.bytes
}

func (b *Broadcaster) unsubscribe(s *Stream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, s)
}

// Stream is one reader's view of the call audio. Frames are shared, read-only
// slices: readers must not modify them. Use Next for frame-at-a-time access, or
// Read to treat the audio as a continuous byte stream. A Stream is meant to be
// consumed by a single goroutine.
type Stream struct {
	b    *Broadcaster
	mu   sync.Mutex
	cond *sync.Cond
	q    [][]byte
	eof  bool
	gone bool

	partial []byte // leftover of a frame partially consumed by Read
}

func (s *Stream) push(frame []byte) {
	s.mu.Lock()
	if !s.gone {
		s.q = append(s.q, frame)
	}
	s.mu.Unlock()
	s.cond.Signal()
}

func (s *Stream) finish() {
	s.mu.Lock()
	s.eof = true
	s.mu.Unlock()
	s.cond.Broadcast()
}

// Next returns the next frame, blocking until one is available. It returns
// io.EOF after the call ends and all frames have been delivered, or ctx.Err()
// if ctx is cancelled first.
func (s *Stream) Next(ctx context.Context) ([]byte, error) {
	if len(s.partial) > 0 {
		f := s.partial
		s.partial = nil
		return f, nil
	}
	// Wake the waiter if ctx is cancelled while blocked.
	stop := context.AfterFunc(ctx, func() {
		// Broadcast under the lock so a waiter can't miss the wakeup.
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.q) == 0 && !s.eof && ctx.Err() == nil {
		s.cond.Wait()
	}
	if len(s.q) > 0 {
		f := s.q[0]
		s.q[0] = nil
		s.q = s.q[1:]
		return f, nil
	}
	if s.eof {
		return nil, io.EOF
	}
	return nil, ctx.Err()
}

// Read implements io.Reader over the concatenated frames. It blocks without a
// context; prefer Next when cancellation matters.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	f, err := s.Next(context.Background())
	if err != nil {
		return 0, err
	}
	n := copy(p, f)
	if n < len(f) {
		s.partial = f[n:]
	}
	return n, nil
}

// Release stops delivery to this stream and frees any queued frames. Call it
// when a reader is done early; it is safe to call more than once.
func (s *Stream) Release() {
	if s.b != nil {
		s.b.unsubscribe(s)
	}
	s.mu.Lock()
	s.gone = true
	s.eof = true
	s.q = nil
	s.partial = nil
	s.mu.Unlock()
	s.cond.Broadcast()
}
