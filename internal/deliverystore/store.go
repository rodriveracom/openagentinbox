// Package deliverystore is the Messenger store v2 prototype (SYNTHETIC DATA ONLY): one SQLite database, one writer, indexed
// per-recipient delivery records, bounded pages with durable cursors, bodies on demand, idempotent accept.
//
// The four states of the operator's directive (2026-10-19 15:57Z):
//
//	ACCEPTED   the message is durably stored (Accept returned);
//	DELIVERED  a client fetched its summary (MarkDelivered);
//	NOTICED    the seat acknowledged it (Notice);
//	OBLIGATION COMPLETED is NOT here: it is Runtime's state, which references message ids.
package deliverystore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA busy_timeout = 5000;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA fullfsync = ON;
PRAGMA checkpoint_fullfsync = ON;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS messages (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  sender TEXT NOT NULL,
  conversation_id TEXT,
  parent_id TEXT,
  subject TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  bytes INTEGER NOT NULL,
  preview TEXT NOT NULL DEFAULT '',
  body BLOB NOT NULL,
  final INTEGER NOT NULL DEFAULT 0,
  created_text TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS deliveries (
  seat TEXT NOT NULL,
  seq INTEGER NOT NULL REFERENCES messages(seq),
  accepted_at INTEGER NOT NULL,
  delivered_at INTEGER,
  noticed_at INTEGER,
  owed INTEGER NOT NULL DEFAULT 0,
  answered_at INTEGER,
  PRIMARY KEY (seat, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS deliveries_unread ON deliveries (seat, seq) WHERE noticed_at IS NULL;
CREATE TABLE IF NOT EXISTS operations (
  op_key TEXT PRIMARY KEY,
  digest BLOB NOT NULL,
  message_id TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS wake_requests (
  assignment_id TEXT PRIMARY KEY REFERENCES operations(op_key),
  seat TEXT NOT NULL,
  requested_at INTEGER NOT NULL,
  requests INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS cursors (
  seat TEXT PRIMARY KEY,
  page_seq INTEGER NOT NULL DEFAULT 0,
  sync_seq INTEGER NOT NULL DEFAULT 0
);
`

var (
	ErrConflict = errors.New("operation key already holds different content")
	ErrNoSuch   = errors.New("no such message for this seat")
	// ErrNotAccepted: a wake or an attempt names an assignment that was never durably accepted. The order is fixed:
	// accept first, then any delivery attempt (the operator's order 2026-10-19 16:13Z).
	ErrNotAccepted = errors.New("assignment was not accepted yet")
)

type Store struct {
	// AlertRecipient also receives the first offline notice of each day for a seat.
	// Empty, the default, sends no such alert. Set it before the store is shared.
	AlertRecipient string

	mailWrites     mailWriteQueue
	insertDelivery *sql.Stmt
	selectUnread   *sql.Stmt
	watch          watchHub
	db             *sql.DB
	readers        *sql.DB
}

// Writer returns the store-owned serialized connection for same-database extensions.
// The Store owns its lifetime and pool configuration.
func (s *Store) Writer() *sql.DB { return s.db }

// Reader returns the bounded read pool used for committed snapshots.
func (s *Store) Reader() *sql.DB { return s.readDB() }

func (s *Store) readDB() *sql.DB {
	if s.readers != nil {
		return s.readers
	}
	return s.db
}

func Open(path string) (*Store, error) { return openStore(path, true) }

// OpenMirror opens the store the way openagentinboxd's mirror needs it: the schema and the read indexes, WITHOUT the reliable-delivery migration (delivery_outbox, mailbox_events, their
// triggers and the one snapshot row per existing delivery). Those exist for the v2 delivery daemon, which is the only user of them and the only thing that would drain the outbox;
// migrating the live -mirror-db for nothing adds triggers to every delivery write and a full-table snapshot in one transaction (council read of PR 145, M2). The daemon opens with Open.
func OpenMirror(path string) (*Store, error) { return openStore(path, false) }

func openStore(path string, reliable bool) (*Store, error) {
	// Configure EVERY connection, including replacements after a cancelled query.
	q := url.Values{}
	q.Set("_txlock", "immediate")
	for _, pragma := range []string{"busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(ON)", "fullfsync(ON)", "checkpoint_fullfsync(ON)", "mmap_size(0)", "temp_store(FILE)", "wal_autocheckpoint(256)"} {
		q.Add("_pragma", pragma)
	}
	writerQuery := q.Encode() + "&_pragma=cache_size(-4096)"
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath(), RawQuery: writerQuery}
	if path == ":memory:" {
		u.Opaque = ":memory:"
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // Transactions serialize on this connection.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db, reliable); err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{db: db, watch: newWatchHub()}
	if err := st.migrateDisplay(); err != nil {
		db.Close()
		return nil, err
	}
	if err := st.MigrateSOR(); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		q.Del("_txlock")
		q.Add("mode", "ro")
		q.Add("_pragma", "query_only(ON)")
		q.Add("_pragma", "cache_size(-1024)")
		u.RawQuery = q.Encode()
		rd, err := sql.Open("sqlite", u.String())
		if err != nil {
			db.Close()
			return nil, err
		}
		rd.SetMaxOpenConns(4)
		rd.SetMaxIdleConns(4)
		if err := rd.Ping(); err != nil {
			rd.Close()
			db.Close()
			return nil, err
		}
		st.readers = rd
	}
	// Prepare after all migrations. Compiling the delivery INSERT also compiles its
	// trigger programs; the single writer can reuse them across transactions.
	st.insertDelivery, err = db.Prepare(`INSERT INTO deliveries(seat,seq,accepted_at,delivered_at,owed,priority,created_at) VALUES(?,?,?,?,?,?,?)`)
	if err == nil {
		st.selectUnread, err = st.readDB().Prepare(`SELECT m.seq, m.id, m.sender, m.subject, m.priority, m.preview, m.bytes, m.created_at, d.delivered_at IS NOT NULL
	  FROM deliveries d INDEXED BY deliveries_unread JOIN messages m ON m.seq = d.seq
	  WHERE d.seat = ? AND d.noticed_at IS NULL AND d.seq > ? ORDER BY d.seq LIMIT ?`)
	}
	if err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// migrate brings a database made before the pending view up to date, in one transaction and idempotently: it adds
// deliveries.owed (a p0/p1 the recipient did not send) and deliveries.answered_at, backfills owed from the stored
// priority, and creates deliveries_pending. READING (noticed_at) never touches these: a request stays pending after it
// is read and after a restart until the seat answers it.
func migrate(db *sql.DB, reliable bool) (err error) {
	// BEGIN IMMEDIATE takes the write lock up front: two processes opening the same old database serialise here (the second waits for
	// busy_timeout, then finds the columns present) instead of both reading "no column" and failing on a duplicate ALTER or BUSY.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	have := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info('deliveries')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	mhave := map[string]bool{}
	mrows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info('messages')`)
	if err != nil {
		return err
	}
	for mrows.Next() {
		var n string
		if err := mrows.Scan(&n); err != nil {
			mrows.Close()
			return err
		}
		mhave[n] = true
	}
	mrows.Close()
	if !mhave["final"] {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN final INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !mhave["created_text"] {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN created_text TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !have["owed"] {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE deliveries ADD COLUMN owed INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE deliveries SET owed = 1 WHERE seq IN (SELECT m.seq FROM messages m WHERE m.priority <= 1)`); err != nil {
			return err
		}
	}
	if !have["answered_at"] {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE deliveries ADD COLUMN answered_at INTEGER`); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries (seat, seq) WHERE owed = 1 AND answered_at IS NULL`); err != nil {
		return err
	}
	if err := migrateReadIndexes(ctx, conn, have); err != nil {
		return err
	}
	if reliable {
		if err := migrateReliable(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS activity(seat TEXT NOT NULL,activity_key TEXT NOT NULL,task_ref TEXT NOT NULL,state TEXT NOT NULL,updated_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,PRIMARY KEY(seat,activity_key)) WITHOUT ROWID`); err != nil {
		return err
	}
	if reliable { // the attention projection (triggers on deliveries, a state table) belongs with the reliable-delivery tables it reads; the mirror's open (OpenMirror) takes neither
		if err := migrateAttention(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// owes reports whether delivering m to seat creates an obligation to answer: p0 and p1 do, p2 does not. A message addressed to its own sender
// is owed too: the CLI lists it as pending (Architect's ruling 2026-10-26, after the Phase A fidelity check found three such messages), so the
// store keeps that contract rather than excluding the sender.
func owes(m Message, seat string) int {
	if m.Priority <= 1 && !m.Final {
		return 1
	}
	return 0
}

func (s *Store) Close() error {
	var e error
	if s.readers != nil {
		e = s.readers.Close()
	}
	return errors.Join(e, s.db.Close())
}

// Sender returns the sender of the message with this id, and false when the store does not hold it (yet).
func (s *Store) Sender(id string) (string, bool, error) {
	var sender string
	err := s.readDB().QueryRow(`SELECT sender FROM messages WHERE id = ?`, id).Scan(&sender)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return sender, err == nil, err
}

type Message struct {
	ThreadHint string // Derived by the transport; excluded from the submission digest.

	ID, Sender, ConversationID, ParentID, Subject string
	Intent                                        string
	Priority                                      int
	Final                                         bool // the sender expects no answer: never owed
	Recipients                                    []string
	Body                                          []byte
	ExternalPayloadDigest                         []byte // Optional digest of transport-specific content committed by the acceptance callback.
}

// digest frames every field with its length, so no two different messages share a digest (("a\0b","c") and ("a","b\0c")
// differ, as do recipient lists ["a,b"] and ["a","b"]).
func digest(m Message) []byte {
	h := sha256.New()
	field := func(b []byte) { fmt.Fprintf(h, "%d:", len(b)); h.Write(b) }
	for _, f := range []string{m.Sender, m.ConversationID, m.ParentID, m.Subject, fmt.Sprint(m.Priority)} {
		field([]byte(f))
	}
	fmt.Fprintf(h, "r%d:", len(m.Recipients))
	for _, r := range m.Recipients {
		field([]byte(r))
	}
	field(m.Body)
	if m.Final { // only when set, so the digest of every message accepted before the field existed is unchanged
		field([]byte("final"))
	}
	if m.Intent != "" {
		field([]byte("intent/" + m.Intent))
	}
	if len(m.ExternalPayloadDigest) > 0 {
		field([]byte("transport"))
		field(m.ExternalPayloadDigest)
	}
	return h.Sum(nil)
}

// Accept stores a message for its recipients in ONE transaction. opKey makes it idempotent: the same key and content
// returns the prior message id; the same key with different content is ErrConflict.
func (s *Store) Accept(opKey string, m Message, now time.Time) (string, error) {
	return s.AcceptContext(context.Background(), opKey, m, now)
}

// AcceptContext is Accept with cancellation while waiting for storage or executing SQL.
func (s *Store) AcceptContext(ctx context.Context, opKey string, m Message, now time.Time) (string, error) {
	return s.AcceptWithReceipt(ctx, opKey, m, now, nil)
}

// AcceptWithReceipt atomically applies delivery-specific bookkeeping on first insertion.
func (s *Store) AcceptWithReceipt(ctx context.Context, opKey string, m Message, now time.Time, record func(*sql.Tx) error) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, inserted, err := s.acceptTx(ctx, tx, opKey, m, now, nil)
	if err != nil {
		return "", err
	}
	if !inserted {
		return id, nil
	}
	if record != nil {
		if err = record(tx); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	s.signal(m.Recipients)
	return id, nil
}

func (s *Store) acceptTx(ctx context.Context, tx *sql.Tx, opKey string, m Message, now time.Time, sequence any) (string, bool, error) {
	dg := digest(m)
	var prevID string
	var prevDg []byte
	err := tx.QueryRowContext(ctx, `SELECT message_id, digest FROM operations WHERE op_key = ?`, opKey).Scan(&prevID, &prevDg)
	if err == nil {
		if string(prevDg) != string(dg) {
			return "", false, ErrConflict
		}
		return prevID, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	prev := previewPrefix(m.Body)
	conversation := m.ConversationID
	if conversation == "" {
		conversation = m.ThreadHint
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages (seq,id, sender, conversation_id, parent_id, subject, priority, created_at, bytes, preview, body, final, created_text, display_body)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, sequence, m.ID, m.Sender, conversation, m.ParentID, m.Subject, m.Priority, now.UnixMilli(), len(m.Body), prev, m.Body, b2i(m.Final), now.UTC().Format(time.RFC3339Nano), DisplayBody(m.Body))
	if err != nil {
		return "", false, err
	}
	seq, _ := res.LastInsertId()
	delivery := tx.StmtContext(ctx, s.insertDelivery)
	for _, r := range m.Recipients {
		if _, err := delivery.ExecContext(ctx, r, seq, now.UnixMilli(), nil, owes(m, r), m.Priority, now.UnixMilli()); err != nil {
			return "", false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations (op_key, digest, message_id, created_at) VALUES (?,?,?,?)`, opKey, dg, m.ID, now.UnixMilli()); err != nil {
		return "", false, err
	}
	return m.ID, true, nil
}

// AcceptBatch is the load path of the harness: many messages in one transaction (prepared statements).
func (s *Store) AcceptBatch(msgs []Message, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	im, err := tx.Prepare(`INSERT INTO messages (id, sender, subject, priority, created_at, bytes, preview, body, final, created_text, conversation_id, parent_id, display_body) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer im.Close()
	id, err := tx.Prepare(`INSERT INTO deliveries (seat, seq, accepted_at, owed, priority) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer id.Close()
	for _, m := range msgs {
		prev := previewPrefix(m.Body)
		res, err := im.Exec(m.ID, m.Sender, m.Subject, m.Priority, now.UnixMilli(), len(m.Body), prev, m.Body, b2i(m.Final), now.UTC().Format(time.RFC3339Nano), m.ConversationID, m.ParentID, DisplayBody(m.Body))
		if err != nil {
			return err
		}
		seq, _ := res.LastInsertId()
		for _, r := range m.Recipients {
			if _, err := id.Exec(r, seq, now.UnixMilli(), owes(m, r), m.Priority); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type Summary struct {
	Seq          int64
	ID, Sender   string
	Subject      string
	Priority     int
	Preview      string
	Bytes        int
	Delivered    bool
	CreatedMilli int64
}

// Unread returns up to limit unread summaries for seat with seq > after, oldest first, NEVER a body. next is the seq to
// pass as `after` for the following page (0 when there is no more). Cost: one index range scan of at most limit rows.
func (s *Store) Unread(seat string, after int64, limit int) (rows []Summary, next int64, err error) {
	return s.UnreadContext(context.Background(), seat, after, limit)
}

func (s *Store) UnreadContext(ctx context.Context, seat string, after int64, limit int) (rows []Summary, next int64, err error) {
	if limit < 1 || limit > 50 {
		limit = 50
	}
	q, err := s.unreadRows(ctx, seat, after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer q.Close()
	for q.Next() {
		r, err := scanUnread(q)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
	}
	if len(rows) > limit {
		next = rows[limit-1].Seq
		rows = rows[:limit]
	}
	return rows, next, q.Err()
}

func (s *Store) unreadRows(ctx context.Context, seat string, after int64, limit int) (*sql.Rows, error) {
	return s.selectUnread.QueryContext(ctx, seat, after, limit+1)
}
func scanUnread(q *sql.Rows) (Summary, error) {
	var r Summary
	err := q.Scan(&r.Seq, &r.ID, &r.Sender, &r.Subject, &r.Priority, &r.Preview, &r.Bytes, &r.CreatedMilli, &r.Delivered)
	return r, err
}

// MarkDelivered records that a client fetched these summaries (DELIVERED). Idempotent.
func (s *Store) MarkDelivered(seat string, seqs []int64, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range seqs {
		if _, err := tx.Exec(`UPDATE deliveries SET delivered_at = COALESCE(delivered_at, ?) WHERE seat = ? AND seq = ?`, now.UnixMilli(), seat, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Notice records that the seat acknowledged (NOTICED). Idempotent; fetching a body never notices.
func (s *Store) Notice(seat, id string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE deliveries SET noticed_at = COALESCE(noticed_at, ?) WHERE seat = ? AND seq = (SELECT seq FROM messages WHERE id = ?)`, now.UnixMilli(), seat, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuch
	}
	return nil
}

// Answer records that the seat answered the message (ANSWERED, distinct from NOTICED). Idempotent; a message that
// created no obligation is accepted and changes nothing visible.
func (s *Store) Answer(seat, id string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE deliveries SET answered_at = COALESCE(answered_at, ?) WHERE seat = ? AND seq = (SELECT seq FROM messages WHERE id = ?)`, now.UnixMilli(), seat, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuch
	}
	return nil
}

// Pending returns up to limit unanswered p0/p1 requests for seat with seq > after, oldest first, read or not, NEVER a
// body, with the same paging contract as Unread. Cost: one range scan of the partial index deliveries_pending.
func (s *Store) Pending(seat string, after int64, limit int) (rows []Summary, next int64, err error) {
	return s.PendingContext(context.Background(), seat, after, limit)
}

func (s *Store) PendingContext(ctx context.Context, seat string, after int64, limit int) (rows []Summary, next int64, err error) {
	if limit < 1 || limit > 50 {
		limit = 50
	}
	q, err := s.readDB().QueryContext(ctx, `SELECT m.seq, m.id, m.sender, m.subject, m.priority, m.preview, m.bytes, m.created_at, d.delivered_at IS NOT NULL
	  FROM deliveries d INDEXED BY deliveries_pending JOIN messages m ON m.seq = d.seq
	  WHERE d.seat = ? AND d.owed = 1 AND d.answered_at IS NULL AND d.seq > ? ORDER BY d.seq LIMIT ?`, seat, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer q.Close()
	for q.Next() {
		var r Summary
		if err := q.Scan(&r.Seq, &r.ID, &r.Sender, &r.Subject, &r.Priority, &r.Preview, &r.Bytes, &r.CreatedMilli, &r.Delivered); err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
	}
	if len(rows) > limit {
		next = rows[limit-1].Seq
		rows = rows[:limit]
	}
	return rows, next, q.Err()
}

// PendingCount reads the transactionally maintained exact total for this seat.
func (s *Store) PendingCount(seat string) (n int, err error) {
	err = s.readDB().QueryRow(`SELECT COALESCE((SELECT pending FROM mailbox_counts WHERE seat = ?), 0)`, seat).Scan(&n)
	return n, err
}

// Body fetches one body on demand, only for a message addressed to seat. It changes no state.
func (s *Store) Body(seat, id string) ([]byte, error) {
	var b []byte
	err := s.readDB().QueryRow(`SELECT m.body FROM messages m JOIN deliveries d ON d.seq = m.seq WHERE m.id = ? AND d.seat = ?`, id, seat).Scan(&b)
	if err == sql.ErrNoRows {
		return nil, ErrNoSuch
	}
	return b, err
}

// SetCursor and Cursor are the durable per-seat cursors (page and sync positions).
func (s *Store) SetCursor(seat string, page, sync int64) error {
	_, err := s.db.Exec(`INSERT INTO cursors (seat, page_seq, sync_seq) VALUES (?,?,?) ON CONFLICT(seat) DO UPDATE SET page_seq = excluded.page_seq, sync_seq = excluded.sync_seq`, seat, page, sync)
	return err
}

func (s *Store) Cursor(seat string) (page, sync int64, err error) {
	err = s.readDB().QueryRow(`SELECT page_seq, sync_seq FROM cursors WHERE seat = ?`, seat).Scan(&page, &sync)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return
}

// UnreadCount reads the transactionally maintained exact unread total.
func (s *Store) UnreadCount(seat string) (n int, err error) {
	err = s.readDB().QueryRow(`SELECT COALESCE((SELECT unread FROM mailbox_counts WHERE seat = ?), 0)`, seat).Scan(&n)
	return
}

// Backup writes a consistent online copy (VACUUM INTO), the restore point of the drill.
func (s *Store) Backup(dst string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, dst)
	return err
}

// VerifyBackup opens a backup copy read-only and checks it: SQLite integrity_check ok, the schema's tables present, and
// (when requireNonEmpty) at least one message. It returns the message count. A drill counts only on a verified backup.
// integritySQL is the integrity query; a test replaces it to make SQLite ANSWER with a problem row.
var integritySQL = `PRAGMA integrity_check`

func VerifyBackup(path string, requireNonEmpty bool) (int64, error) {
	// the path goes into the URI percent-encoded: a ? or # in it must not open another file
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath(), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(integritySQL).Scan(&res); err != nil {
		return 0, err
	}
	if res != "ok" {
		return 0, fmt.Errorf("integrity_check: %s", res)
	}
	for _, tb := range []string{"messages", "deliveries", "operations", "wake_requests", "cursors"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, tb).Scan(&n); err != nil || n != 1 {
			return 0, fmt.Errorf("the backup has no table %s", tb)
		}
	}
	var msgs int64
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgs); err != nil {
		return 0, err
	}
	if requireNonEmpty && msgs == 0 {
		return 0, errors.New("the backup holds no messages: an empty database does not prove a restore")
	}
	return msgs, nil
}

// NoticeBefore acknowledges every unread delivery of seat up to and including seq (the bulk form of Notice, used for
// history and by the harness).
func (s *Store) NoticeBefore(seat string, seq int64, now time.Time) error {
	_, err := s.db.Exec(`UPDATE deliveries SET noticed_at = ?, delivered_at = COALESCE(delivered_at, ?) WHERE seat = ? AND seq <= ? AND noticed_at IS NULL`, now.UnixMilli(), now.UnixMilli(), seat, seq)
	return err
}

// MaxSeq returns the highest message seq.
func (s *Store) MaxSeq() (n int64, err error) {
	err = s.readDB().QueryRow(`SELECT COALESCE(MAX(seq),0) FROM messages`).Scan(&n)
	return
}

// UnreadThreshold returns the seq of the delivery that is `keep` places from the newest for seat (0 if fewer).
func (s *Store) UnreadThreshold(seat string, keep int) (n int64, err error) {
	err = s.readDB().QueryRow(`SELECT COALESCE((SELECT seq FROM deliveries WHERE seat = ? ORDER BY seq DESC LIMIT 1 OFFSET ?), 0)`, seat, keep).Scan(&n)
	return
}

// RequestWake records that a wake was requested for an accepted assignment. It coalesces: any number of requests for
// one assignment id are ONE row (created is true only the first time) and a counter. Refused before the accept.
func (s *Store) RequestWake(assignmentID, seat string, now time.Time) (created bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM operations WHERE op_key = ?`, assignmentID).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, ErrNotAccepted
	}
	res, err := tx.Exec(`INSERT INTO wake_requests (assignment_id, seat, requested_at) VALUES (?,?,?) ON CONFLICT(assignment_id) DO NOTHING`, assignmentID, seat, now.UnixMilli())
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		if _, err := tx.Exec(`UPDATE wake_requests SET requests = requests + 1 WHERE assignment_id = ?`, assignmentID); err != nil {
			return false, err
		}
	}
	return rows == 1, tx.Commit()
}

// WakeRequests returns how many requests were made for the assignment (0 if none).
func (s *Store) WakeRequests(assignmentID string) (n int, err error) {
	err = s.readDB().QueryRow(`SELECT COALESCE((SELECT requests FROM wake_requests WHERE assignment_id = ?), 0)`, assignmentID).Scan(&n)
	return
}

// DeliveryNotice is what Messenger tells the Runtime adapter about an assignment: its state in Messenger's own
// domain (accepted, delivered to the seat's client, noticed = acknowledged), the digest the store computed at accept,
// and how many wake requests were coalesced. Submission evidence (session_started, client_ready, input_submitted) is
// Runtime's journal, NOT recorded here, so there is one authority for it.
type DeliveryNotice struct {
	State        string
	Digest       string
	WakeRequests int
}

func (s *Store) DeliveryNotice(assignmentID, seat string) (DeliveryNotice, error) {
	var n DeliveryNotice
	var dg []byte
	var delivered, noticed sql.NullInt64
	err := s.readDB().QueryRow(`SELECT o.digest, d.delivered_at, d.noticed_at
	  FROM operations o JOIN messages m ON m.id = o.message_id JOIN deliveries d ON d.seq = m.seq AND d.seat = ?
	  WHERE o.op_key = ?`, seat, assignmentID).Scan(&dg, &delivered, &noticed)
	if err == sql.ErrNoRows {
		return n, ErrNotAccepted
	}
	if err != nil {
		return n, err
	}
	n.Digest = fmt.Sprintf("%x", dg)
	n.State = "accepted"
	if delivered.Valid {
		n.State = "delivered"
	}
	if noticed.Valid {
		n.State = "noticed"
	}
	n.WakeRequests, err = s.WakeRequests(assignmentID)
	return n, err
}

// NoticeAssignment is the seat's acknowledgement of one EXACT assignment envelope: the assignment id (the operation
// key, which carries the revision as "<assignment>/r<n>"), the immutable digest stored at accept, and the seat. A
// digest that is not the stored one is ErrConflict and changes nothing: an ACK of a different payload is not an ACK of
// this assignment (PR 979 contract: ACK identifies the exact envelope, not provider transport success).
func (s *Store) NoticeAssignment(assignmentID, digestHex, seat string, now time.Time) error {
	var dg []byte
	var id string
	err := s.readDB().QueryRow(`SELECT digest, message_id FROM operations WHERE op_key = ?`, assignmentID).Scan(&dg, &id)
	if err == sql.ErrNoRows {
		return ErrNotAccepted
	}
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", dg) != digestHex {
		return ErrConflict
	}
	return s.Notice(seat, id, now)
}
