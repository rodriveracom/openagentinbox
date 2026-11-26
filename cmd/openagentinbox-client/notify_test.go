package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeliverAdvancesOnlyAfterSuccessfulWake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/agent/notice" || r.URL.Query().Get("after") != "0" {
			t.Errorf("unexpected notice request: %s %s", r.Method, r.URL.RequestURI())
			http.Error(w, "bad notice request", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(noticeHead{Cursor: 31, Count: 2})
	}))
	defer server.Close()
	p := profile{Origin: server.URL, Name: "fixture-seat"}
	wakeError := errors.New("fixture wake failed")
	if err := deliver(t.Context(), path, p, func(string) error { return wakeError }); !errors.Is(err, wakeError) {
		t.Fatalf("failed wake: %v", err)
	}
	assertCursor := func(want int64) {
		t.Helper()
		err := stateAt(path, func(s *noticeState) error {
			if s.Delivered != want {
				t.Errorf("recorded cursor = %d, want %d", s.Delivered, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertCursor(0)

	if err := deliver(t.Context(), path, p, func(notice string) error {
		if !strings.HasPrefix(notice, "2 new: OPENAGENTINBOX_PROFILE="+shellQuote(path)+" ") || !strings.HasSuffix(notice, " inbox") {
			t.Errorf("unexpected notification: %q", notice)
		}
		return nil
	}); err != nil {
		t.Fatalf("successful wake: %v", err)
	}
	assertCursor(31)

	if err := deliver(t.Context(), path, p, func(string) error {
		t.Error("sent another wake inside the cooldown")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("notice requests = %d, want 2", got)
	}
}

func TestNoticeReadyFocusAndDigest(t *testing.T) {
	focus := noticeState{Mode: "focus", Until: 100}
	if focus.ready(99) || !focus.ready(100) || focus.Mode != "available" || focus.Until != 0 {
		t.Fatalf("unexpected focus transition: %+v", focus)
	}
	digest := noticeState{Mode: "digest", First: 100, Interval: 60, Last: 110}
	if digest.ready(159) || digest.ready(169) || !digest.ready(170) {
		t.Fatal("digest must respect both interval and cooldown")
	}
}

func TestNoticeForAStandardSeatNamesOnlyTheSeat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config/openagentinbox/seats", "fixture-seat.json")
	got := noticeText(profile{Name: "fixture-seat"}, path, noticeHead{Cursor: 7, Count: 101})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got != "100+ new: "+clientCommand(exe, home)+" --seat fixture-seat inbox" {
		t.Fatalf("notice = %q", got)
	}
}

func TestNoticeNamesTheRunningClientNotAnUnrelatedAlias(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "openagentinbox-client")
	alias := filepath.Join(bin, "oai")
	for _, file := range []string{exe, alias} {
		if err := os.WriteFile(file, []byte(file), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Something else already called oai is never named.
	if got := clientCommand(exe, home); got != "~/.local/bin/openagentinbox-client" {
		t.Fatalf("unrelated alias: %q", got)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("openagentinbox-client", alias); err != nil {
		t.Fatal(err)
	}
	if got := clientCommand(exe, home); got != "~/.local/bin/oai" {
		t.Fatalf("own alias: %q", got)
	}
	if got := clientCommand(exe, "/elsewhere"); got != alias && got != shellQuote(alias) {
		t.Fatalf("outside home: %q", got)
	}
}
