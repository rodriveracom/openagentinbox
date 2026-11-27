package deliverystore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Away struct {
	Seat          string
	Until         time.Time
	Note, Forward string
}

func (s *Store) SetAway(ctx context.Context, a Away) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = checkFenceTx(ctx, tx); e != nil {
		return e
	}
	if a.Until.IsZero() {
		_, e = tx.ExecContext(ctx, "DELETE FROM away_state WHERE seat=?", a.Seat)
	} else {
		_, e = tx.ExecContext(ctx, "INSERT INTO away_state VALUES(?,?,?,?) ON CONFLICT(seat) DO UPDATE SET until_ms=excluded.until_ms,note=excluded.note,forward=excluded.forward", a.Seat, a.Until.UnixMilli(), a.Note, a.Forward)
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Away(ctx context.Context, seat string, now time.Time) (Away, error) {
	var a Away
	var ms int64
	q, e := s.readDB().QueryContext(ctx, "SELECT seat,until_ms,note,forward FROM away_state WHERE seat=? AND until_ms>?", seat, now.UnixMilli())
	if e != nil {
		return a, e
	}
	defer q.Close()
	if q.Next() {
		if e = q.Scan(&a.Seat, &ms, &a.Note, &a.Forward); e != nil {
			return a, e
		}
		a.Until = time.UnixMilli(ms).UTC()
	}
	return a, q.Err()
}

type AutoResponse struct{ Seat, Kind, Detail string }

func (s *Store) AcceptWithAuto(ctx context.Context, m Message, now time.Time, budget int, automatic []AutoResponse) (Stored, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Stored{}, e
	}
	defer tx.Rollback()
	accepted, e := s.acceptRecordTx(ctx, tx, m, now, budget)
	if e != nil {
		return Stored{}, e
	}
	if accepted.Repeated {
		return accepted, nil
	}
	var senderNotice, alerted bool
	for _, a := range automatic {
		if m.Priority == 2 || m.Sender == "postmaster" || strings.Contains(string(m.Body), "\nAuto:") {
			break
		}
		if a.Kind != "away" && a.Kind != "offline" && a.Kind != "closed" {
			return Stored{}, errors.New("invalid automatic response")
		}
		day := now.UTC().Format(time.DateOnly)
		// The marker outlives message retention and commits with both deliveries.
		r, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO auto_once VALUES(?,?,?,?)", m.Sender, a.Seat, a.Kind, day)
		if e != nil {
			return Stored{}, e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return Stored{}, e
		}
		if n == 0 {
			continue
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join([]string{day, m.Sender, a.Seat, a.Kind}, "\x00"))))
		auto := Message{ID: "msg-postmaster-" + key, Sender: "postmaster", ParentID: m.ID, Recipients: []string{m.Sender}, Final: true, Priority: 2, Subject: "Automatic " + a.Kind + " notice", Body: []byte("Priority: p2\nFinal: yes\nAuto: " + a.Kind + "\n\n" + m.ID + " for " + a.Seat + ": " + a.Detail)}
		if _, e = s.acceptRecordTx(ctx, tx, auto, now, 0); e != nil {
			return Stored{}, e
		}
		senderNotice = true
		if a.Kind == "offline" && s.AlertRecipient != "" {
			marker, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO auto_once VALUES(?,?,?,?)", "postmaster", a.Seat, "offline-alert", day)
			if e != nil {
				return Stored{}, e
			}
			count, e := marker.RowsAffected()
			if e != nil {
				return Stored{}, e
			}
			if count > 0 {
				alerted = true
				auto.ID = "msg-postmaster-alert-" + key
				auto.Recipients = []string{s.AlertRecipient}
				if _, e = s.acceptRecordTx(ctx, tx, auto, now, 0); e != nil {
					return Stored{}, e
				}
			}
		}

	}
	if e = tx.Commit(); e != nil {
		return Stored{}, e
	}
	s.signal(m.Recipients)
	if senderNotice {
		s.signal([]string{m.Sender})
	}
	if alerted {
		s.signal([]string{s.AlertRecipient})
	}
	return accepted, nil
}
