package audio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestFanOutAndEOF(t *testing.T) {
	var sink bytes.Buffer
	b := NewBroadcaster(&sink)
	a, c := b.Subscribe(), b.Subscribe()

	frame := []byte("abc")
	for i := 0; i < 3; i++ {
		if err := b.Publish(frame); err != nil {
			t.Fatal(err)
		}
	}
	frame[0] = 'X' // caller reuses buffer; subscribers must be unaffected
	b.Close()

	for _, s := range []*Stream{a, c} {
		got, err := io.ReadAll(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "abcabcabc" {
			t.Fatalf("got %q", got)
		}
	}
	if sink.String() != "abcabcabc" {
		t.Fatalf("sink got %q", sink.String())
	}
	if f, n := b.Stats(); f != 3 || n != 9 {
		t.Fatalf("stats = %d frames %d bytes", f, n)
	}
	if err := b.Publish(frame); !errors.Is(err, ErrClosed) {
		t.Fatalf("publish after close: %v", err)
	}
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	b := NewBroadcaster(nil)
	slow := b.Subscribe() // never read until the end
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			_ = b.Publish(make([]byte, 160))
		}
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher blocked by slow subscriber")
	}
	n := 0
	for {
		_, err := slow.Next(context.Background())
		if err == io.EOF {
			break
		}
		n++
	}
	if n != 10000 {
		t.Fatalf("slow subscriber got %d frames", n)
	}
}

func TestNextBlocksUntilFrameAndHonorsContext(t *testing.T) {
	b := NewBroadcaster(nil)
	s := b.Subscribe()

	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = b.Publish([]byte("x"))
	}()
	f, err := s.Next(context.Background())
	if err != nil || string(f) != "x" {
		t.Fatalf("got %q %v", f, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
}

func TestReleaseAndLateSubscribe(t *testing.T) {
	b := NewBroadcaster(nil)
	s := b.Subscribe()
	_ = b.Publish([]byte("a"))
	s.Release()
	_ = b.Publish([]byte("b"))
	if _, err := s.Next(context.Background()); err != io.EOF {
		t.Fatalf("released stream: %v", err)
	}
	b.Close()
	if _, err := b.Subscribe().Next(context.Background()); err != io.EOF {
		t.Fatalf("subscribe after close: %v", err)
	}
}
