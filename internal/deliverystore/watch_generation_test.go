package deliverystore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCommittedGenerationsAndSeatIsolation(t *testing.T) {
	s := nativeStore(t)
	ctx := context.Background()
	signals := func(seat string, native bool) *Subscription {
		t.Helper()
		w, err := s.WatchSeat(seat, native)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Close)
		return w
	}
	check := func(ch <-chan struct{}, want bool) {
		t.Helper()
		select {
		case <-ch:
			if !want {
				t.Fatal("unexpected wake")
			}
		default:
			if want {
				t.Fatal("missed committed delivery")
			}
		}
	}
	native, http := signals("b", true), signals("b", false)
	other := signals("c", false).Changes()
	first := native.Changes()
	if first != http.Changes() {
		t.Fatal("readers have independent generations")
	}
	now := time.Now()
	msg := nativeMessage("v2")
	if _, err := s.AcceptContext(ctx, "op", msg, now); err != nil {
		t.Fatal(err)
	}
	check(first, true)
	if err := http.Rearm(); err != nil {
		t.Fatal(err)
	}
	next := http.Changes()
	if _, err := s.AcceptContext(ctx, "op", msg, now); err != nil {
		t.Fatal(err)
	}
	check(next, false)
	if _, err := s.AcceptRecord(ctx, nativeMessage("native"), now); err != nil {
		t.Fatal(err)
	}
	check(next, true)
	check(other, false)

	for _, seat := range []string{"operator", "postmaster"} {
		if err := s.PutSeat(SeatRec{Seat: seat}); err != nil {
			t.Fatal(err)
		}
	}
	s.AlertRecipient = "operator"
	alert := signals("operator", false)
	sender := signals("a", true)
	ach, sch := alert.Changes(), sender.Changes()
	auto := []AutoResponse{{Seat: "b", Kind: "offline"}}
	for _, id := range []string{"offline1", "offline2"} {
		if _, err := s.AcceptWithAuto(ctx, nativeMessage(id), now, 0, auto); err != nil {
			t.Fatal(err)
		}
		check(ach, id == "offline1")
		check(sch, id == "offline1")
		if err := alert.Rearm(); err != nil {
			t.Fatal(err)
		}
		if err := sender.Rearm(); err != nil {
			t.Fatal(err)
		}
		ach, sch = alert.Changes(), sender.Changes()
	}
}

// A burst to eight slow readers that already have a wake outstanding.
func BenchmarkCoalescedWake(b *testing.B) {
	s := &Store{watch: newWatchHub()}
	for i := 0; i < 8; i++ {
		w, err := s.WatchSeat("seat", false)
		if err != nil {
			b.Fatal(err)
		}
		defer w.Close()
		w.Changes()
	}
	seats := []string{"seat"}
	s.signal(seats)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.signal(seats)
	}
}

// All 512 slots remain reserved while independent readers refresh generations.
func TestDostoevskyRearmAtCapacity(t *testing.T) {
	s := &Store{watch: newWatchHub()}
	var watches []*Subscription
	for i := 0; i < MaxStoreWaiters; i++ {
		w, e := s.WatchSeat(fmt.Sprintf("recipient-%d", i/MaxSeatWaiters), i < maxNativeWaiters)
		if e != nil {
			t.Fatal(e)
		}
		watches = append(watches, w)
	}
	if _, e := s.WatchSeat("extra", false); e == nil {
		t.Fatal("overflow admitted")
	}
	keys := make([]string, MaxStoreWaiters/MaxSeatWaiters)
	for i := range keys {
		keys[i] = fmt.Sprintf("recipient-%d", i)
	}
	for round := 0; round < 10; round++ {
		s.signal(keys)
		var wg sync.WaitGroup
		for _, w := range watches {
			wg.Add(1)
			go func(w *Subscription) {
				defer wg.Done()
				select {
				case <-w.Changes():
				default:
					t.Error("missed commit")
					return
				}
				if err := w.Rearm(); err != nil {
					t.Error(err)
				}
			}(w)
		}
		wg.Wait()
	}
	for _, w := range watches {
		w.Close()
		w.Close()
	}
	if len(s.watch.seats) != 0 || s.watch.native != 0 {
		t.Fatal("watcher reservations leaked")
	}
}
