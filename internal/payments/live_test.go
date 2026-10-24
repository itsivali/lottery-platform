package payments

import (
	"errors"
	"testing"
)

func TestLive_DeliversOnlyToThePlayersOwnSubscribers(t *testing.T) {
	l := NewLive(nil, LiveConfig{})
	a, _ := l.Subscribe("a")
	b, _ := l.Subscribe("b")
	l.send("a", Event{PaymentID: "p1"})
	if ev := <-a.C; ev.PaymentID != "p1" {
		t.Fatalf("a got %v", ev)
	}
	select {
	case ev := <-b.C:
		t.Fatalf("b got a's event %v", ev)
	default:
	}
	l.broadcast(Event{Resync: true})
	if ev := <-a.C; !ev.Resync {
		t.Fatalf("a missed resync: %v", ev)
	}
	if ev := <-b.C; !ev.Resync {
		t.Fatalf("b missed resync: %v", ev)
	}
}

func TestLive_ASubscriberThatFallsBehindIsClosedWithoutBlockingOthers(t *testing.T) {
	l := NewLive(nil, LiveConfig{Buffer: 1})
	slow, _ := l.Subscribe("a")
	fast, _ := l.Subscribe("a")
	l.send("a", Event{PaymentID: "p1"})
	<-fast.C
	l.send("a", Event{PaymentID: "p2"}) // slow still holds p1: it is closed
	if ev := <-fast.C; ev.PaymentID != "p2" {
		t.Fatalf("fast got %v", ev)
	}
	if ev := <-slow.C; ev.PaymentID != "p1" {
		t.Fatalf("slow first = %v", ev)
	}
	if _, ok := <-slow.C; ok {
		t.Fatal("slow subscriber not closed")
	}
	slow.Close() // closing again is harmless
	if _, err := l.Subscribe("a"); err != nil {
		t.Fatalf("place of the closed subscriber not freed: %v", err)
	}
}

func TestLive_CapsAndClose(t *testing.T) {
	l := NewLive(nil, LiveConfig{PerUser: 2, Total: 3})
	s1, _ := l.Subscribe("a")
	_, _ = l.Subscribe("a")
	if _, err := l.Subscribe("a"); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("per-player cap: %v", err)
	}
	_, _ = l.Subscribe("b")
	if _, err := l.Subscribe("c"); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("instance cap: %v", err)
	}
	s1.Close()
	if _, err := l.Subscribe("c"); err != nil {
		t.Fatalf("after close: %v", err)
	}
	l.Close()
	if _, ok := <-s1.C; ok {
		t.Fatal("closed subscriber still open")
	}
	if _, err := l.Subscribe("d"); !errors.Is(err, ErrLiveClosed) {
		t.Fatalf("after shutdown: %v", err)
	}
}
