// Notification state belongs to one local seat profile. Only the delivery cursor,
// timing and preference are persisted; message content never enters a hook prompt.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const notifyHelp = "notify status | start-now | available | focus DURATION | mute | digest DURATION | hook | hooks-config | run -- HARNESS ARGS... | listen codex|claude SESSION_UUID | listen -- WAKE_COMMAND ARGS..."

type noticeState struct {
	Mode      string `json:"mode"`
	Until     int64  `json:"until,omitempty"`
	Interval  int64  `json:"interval_seconds,omitempty"`
	First     int64  `json:"first_pending_at,omitempty"`
	Last      int64  `json:"last_notice_at,omitempty"`
	Delivered int64  `json:"announced_through"`
}
type noticeHead struct {
	Cursor int64
	Count  int
}

var errOccupied = errors.New("seat is busy")

func locked(path string, wait bool, fn func() error) error {
	return lockedFile(path, wait, func(_ *os.File) error { return fn() })
}
func lockedFile(path string, wait bool, fn func(*os.File) error) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	flags := syscall.LOCK_EX
	if !wait {
		flags |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(f.Fd()), flags); e != nil {
		if errors.Is(e, syscall.EWOULDBLOCK) {
			return errOccupied
		}
		return e
	}
	return fn(f)
}
func stateAt(path string, fn func(*noticeState) error) error {
	path += ".notifications"
	return locked(path+".lock", true, func() error {
		s := noticeState{Mode: "available"}
		raw, e := os.ReadFile(path)
		if e == nil {
			if e = json.Unmarshal(raw, &s); e != nil {
				return e
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		before := s
		if e = fn(&s); e != nil || before == s {
			return e
		}
		tmp, e := os.CreateTemp(filepath.Dir(path), ".notify-*")
		if e != nil {
			return e
		}
		defer os.Remove(tmp.Name())
		e = json.NewEncoder(tmp).Encode(s)
		if e == nil {
			e = tmp.Sync()
		}
		closeErr := tmp.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Rename(tmp.Name(), path)
	})
}
func (s *noticeState) ready(now int64) bool {
	switch s.Mode {
	case "mute":
		return false
	case "focus":
		if now < s.Until {
			return false
		}
		s.Mode = "available"
		s.Until = 0
	case "digest":
		if now < s.First+s.Interval {
			return false
		}
	}
	return s.Last == 0 || now >= s.Last+60
}
func head(ctx context.Context, p profile, after int64) (noticeHead, error) {
	var h noticeHead
	b, e := request(ctx, p, "notice?after="+strconv.FormatInt(after, 10), nil)
	if e == nil {
		e = json.Unmarshal(b, &h)
	}
	return h, e
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

var plainPath = regexp.MustCompile("^[A-Za-z0-9._/-]+$")

// clientCommand is how a notice names the running client. A harness may run the
// notice unattended, so it is a path to this program, never a name looked up in
// PATH. The oai alias beside it is used only when it is this same file.
func clientCommand(exe, home string) string {
	alias := filepath.Join(filepath.Dir(exe), "oai")
	self, selfErr := os.Stat(exe)
	other, otherErr := os.Stat(alias)
	if selfErr == nil && otherErr == nil && os.SameFile(self, other) {
		exe = alias
	}
	if rest, ok := strings.CutPrefix(exe, home+"/"); ok && home != "" && plainPath.MatchString(rest) {
		return "~/" + rest
	}
	if plainPath.MatchString(exe) {
		return exe
	}
	return shellQuote(exe)
}

// noticeText is a wake signal only, kept short because every harness pays for it.
// The adapter labels the source, and the inbox itself says whether more pages exist.
func noticeText(p profile, path string, h noticeHead) string {
	count := strconv.Itoa(h.Count)
	if h.Count > 100 {
		count = "100+"
	}
	home, homeErr := os.UserHomeDir()
	name := "openagentinbox-client"
	if exe, err := os.Executable(); err == nil {
		name = clientCommand(exe, home)
	}
	// A profile in the standard seat directory is selected by name alone.
	if homeErr == nil && safeName.MatchString(p.Name) && path == filepath.Join(home, ".config/openagentinbox/seats", p.Name+".json") {
		return fmt.Sprintf("%s new: %s --seat %s inbox", count, name, p.Name)
	}
	return fmt.Sprintf("%s new: OPENAGENTINBOX_PROFILE=%s %s inbox", count, shellQuote(path), name)
}

// Serialize consumers, but keep the preferences lock free while a harness handles mail.
func deliver(ctx context.Context, path string, p profile, emit func(string) error) error {
	return locked(path+".notify.delivery", false, func() error {
		var after int64
		var suppressed bool
		if e := stateAt(path, func(s *noticeState) error {
			after = s.Delivered
			now := time.Now().Unix()
			suppressed = s.Mode == "mute" || s.Mode == "focus" && now < s.Until || s.Last != 0 && now < s.Last+60 || s.First != 0 && !s.ready(now)
			return nil
		}); e != nil || suppressed {
			return e
		}
		h, e := head(ctx, p, after)
		if e != nil || h.Cursor <= after || h.Count == 0 {
			return e
		}
		var ready bool
		e = stateAt(path, func(s *noticeState) error {
			now := time.Now().Unix()
			if s.First == 0 {
				s.First = now
			}
			ready = s.ready(now)
			return nil
		})
		if e != nil || !ready {
			return e
		}
		if e = emit(noticeText(p, path, h)); e != nil {
			return e
		}
		return stateAt(path, func(s *noticeState) error {
			s.Delivered = h.Cursor
			s.Last = time.Now().Unix()
			s.First = 0
			return nil
		})
	})
}
func command(ctx context.Context, p profile, path string, args []string, notice string, lease *os.File) error {
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.ExtraFiles = []*os.File{lease}
	if notice == "" {
		c.Stdin = os.Stdin
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = append(os.Environ(), "OPENAGENTINBOX_PROFILE="+path, "OPENAGENTINBOX_SEAT_NAME="+p.Name, "OPENAGENTINBOX_NOTICE="+notice)
	return c.Run()
}
func pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
func listen(ctx context.Context, path string, p profile, wake func(string, *os.File) error) error {
	return locked(path+".notify.listener", false, func() error {
		delay := time.Second
		pending := false
		var checked time.Time
		lastMode := ""
		occupied := false
		for ctx.Err() == nil {
			var s noticeState
			if e := stateAt(path, func(v *noticeState) error { s = *v; return nil }); e != nil {
				return e
			}
			mode := s.Mode
			if mode == "focus" && s.Until <= time.Now().Unix() {
				mode = "available"
			}
			if occupied && mode == "available" {
				mode = "busy"
			}
			if mode != lastMode || time.Since(checked) >= time.Minute {
				lastMode = mode
				checked = time.Now()
				_, _ = request(ctx, p, "listener", map[string]string{"mode": mode})
			}
			// Suppressed mail stays on the server. Recheck preferences locally without
			// issuing a hot sequence of watch requests against an old cursor.
			if s.Mode == "mute" || s.Mode == "focus" && time.Now().Unix() < s.Until || s.Last != 0 && time.Now().Unix() < s.Last+60 || s.First != 0 && !s.ready(time.Now().Unix()) {
				pause(ctx, 3*time.Second)
				continue
			}
			var e error
			if !pending {
				var b []byte
				b, e = request(ctx, p, "watch?wait=50&after="+strconv.FormatInt(s.Delivered, 10), nil)
				var h noticeHead
				if e == nil {
					e = json.Unmarshal(b, &h)
				}
				pending = e == nil && h.Cursor > s.Delivered
			}
			if e == nil && pending {
				e = lockedFile(path+".notify.turn", false, func(lease *os.File) error {
					return deliver(ctx, path, p, func(msg string) error { return wake(msg, lease) })
				})
				occupied = errors.Is(e, errOccupied)
				if occupied {
					pause(ctx, 3*time.Second)
					continue
				}
				pending = false
			}
			if e != nil {
				if ctx.Err() != nil {
					return nil
				}
				var re remoteError
				if errors.As(e, &re) && (re.status == 401 || re.status == 403) {
					return e
				}
				fmt.Fprintln(os.Stderr, e)
				pause(ctx, delay)
				delay = min(60*time.Second, 2*delay)
			} else {
				delay = time.Second
			}
		}
		return nil
	})
}
func notifications(ctx context.Context, path string, p profile, a []string) error {
	if len(a) == 0 || a[0] == "help" {
		fmt.Println(notifyHelp)
		return nil
	}
	switch a[0] {
	case "start-now":
		if len(a) != 1 {
			return errors.New(notifyHelp)
		}
		return locked(path+".notify.delivery", false, func() error {
			h, e := head(ctx, p, 0)
			if e != nil {
				return e
			}
			return stateAt(path, func(s *noticeState) error { s.Delivered = h.Cursor; s.First = 0; return nil })
		})
	case "status", "available", "mute", "focus", "digest":
		var duration time.Duration
		if a[0] == "focus" || a[0] == "digest" {
			if len(a) != 2 {
				return errors.New(notifyHelp)
			}
			var e error
			duration, e = time.ParseDuration(a[1])
			if e != nil || duration < time.Second || duration > 30*24*time.Hour {
				return errors.New("duration must be between 1s and 720h")
			}
		} else if len(a) != 1 {
			return errors.New(notifyHelp)
		}
		return stateAt(path, func(s *noticeState) error {
			if a[0] != "status" {
				s.Mode = a[0]
				s.Interval = 0
				s.Until = 0
				if s.Mode == "focus" {
					s.Until = time.Now().Add(duration).Unix()
				}
				if s.Mode == "digest" {
					s.Interval = int64(duration / time.Second)
				}
			} else if s.Mode == "focus" && time.Now().Unix() >= s.Until {
				s.Mode = "available"
				s.Until = 0
			}
			return json.NewEncoder(os.Stdout).Encode(s)
		})
	case "run", "listen":
		if a[0] == "listen" && len(a) == 3 && (a[1] == "codex" || a[1] == "claude") {
			if _, err := uuid.Parse(a[2]); err != nil {
				return errors.New("supply the managed seat’s exact session UUID")
			}
			return listen(ctx, path, p, func(msg string, lease *os.File) error {
				args := []string{"codex", "exec", "resume", a[2], msg}
				if a[1] == "claude" {
					args = []string{"claude", "--resume", a[2], "-p", msg}
				}
				return command(ctx, p, path, args, msg, lease)
			})
		}
		if len(a) < 3 || a[1] != "--" {
			return errors.New(notifyHelp)
		}
		if a[0] == "listen" {
			return listen(ctx, path, p, func(msg string, lease *os.File) error { return command(ctx, p, path, a[2:], msg, lease) })
		}
		return lockedFile(path+".notify.turn", false, func(lease *os.File) error { return command(ctx, p, path, a[2:], "", lease) })
	case "hooks-config":
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		if len(a) != 1 {
			return errors.New(notifyHelp)
		}
		cmd := "OPENAGENTINBOX_PROFILE=" + shellQuote(path) + " " + shellQuote(exe) + " notify hook"
		group := []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 5}}}}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"hooks": map[string]any{"SessionStart": group, "Stop": group}})
	case "hook":
		if len(a) != 1 {
			return errors.New(notifyHelp)
		}
		var event struct {
			Event  string `json:"hook_event_name"`
			Active bool   `json:"stop_hook_active"`
		}
		if e := json.NewDecoder(io.LimitReader(os.Stdin, 4<<20)).Decode(&event); e != nil {
			return e
		}
		if (event.Event != "Stop" && event.Event != "SessionStart") || event.Active {
			fmt.Println("{}")
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		emitted := false
		e := deliver(ctx, path, p, func(msg string) error {
			emitted = true
			if event.Event == "Stop" {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"decision": "block", "reason": msg})
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": msg}})
		})
		if e != nil && !errors.Is(e, errOccupied) {
			fmt.Fprintln(os.Stderr, "OpenAgentInbox deferred:", e)
		}
		if !emitted {
			fmt.Println("{}")
		}
		return nil
	}
	return errors.New(notifyHelp)
}
