package usageanalytics

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Fixed defaults; see internal/usageanalytics/sql/README.md.
const (
	queueSize      = 8192
	batchSize      = 500
	flushEvery     = 2 * time.Second
	heartbeatEvery = time.Minute
	replayEvery    = 30 * time.Second
	dbOpTimeout    = 15 * time.Second
	spoolMaxBytes  = 512 << 20
	spoolSuffix    = ".jsonl"
)

type writeFunc func(ctx context.Context, items []item) error

// Sink is a usage.Plugin that writes metadata rows to PostgreSQL in batches,
// with a bounded on-disk spool for database outages.
type Sink struct {
	write    writeFunc
	db       *sql.DB
	spoolDir string
	spoolMax int64
	runID    string
	now      func() time.Time

	ch chan item
	// mu orders enqueue against Close: once Close holds it, no send is in flight.
	mu      sync.RWMutex
	closed  bool
	stop    chan struct{}
	done    chan struct{}
	started atomic.Bool

	queueDropped atomic.Int64
	spoolDropped atomic.Int64
	spooled      atomic.Int64
	writeErrors  atomic.Int64
	spoolBytes   atomic.Int64

	// Owned by the writer goroutine.
	seq     int64
	fileSeq int64
}

// Open creates a sink. It does not connect; the first write does.
// Errors never include the DSN.
func Open(dsn, spoolDir string) (*Sink, error) {
	if strings.TrimSpace(spoolDir) == "" {
		return nil, errors.New("usage analytics: spool directory is required")
	}
	db, errOpen := sql.Open("pgx", dsn)
	if errOpen != nil {
		return nil, errors.New("usage analytics: invalid database DSN")
	}
	// Few connections; idle ones are closed so a dead server is not kept.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	s, errNew := newSink(spoolDir, func(ctx context.Context, items []item) error { return insertItems(ctx, db, items) })
	if errNew != nil {
		_ = db.Close()
		return nil, errNew
	}
	s.db = db
	return s, nil
}

func newSink(spoolDir string, write writeFunc) (*Sink, error) {
	if errMkdir := os.MkdirAll(spoolDir, 0o700); errMkdir != nil {
		return nil, fmt.Errorf("usage analytics: create spool directory: %w", errMkdir)
	}
	s := &Sink{
		write:    write,
		spoolDir: spoolDir,
		spoolMax: spoolMaxBytes,
		runID:    uuid.NewString(),
		now:      time.Now,
		ch:       make(chan item, queueSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	// Quarantined .bad segments stay on disk, so they count toward the cap too.
	files, errList := filepath.Glob(filepath.Join(spoolDir, "*"+spoolSuffix+"*"))
	if errList != nil {
		return nil, errList
	}
	for _, f := range files {
		if info, errStat := os.Stat(f); errStat == nil {
			s.spoolBytes.Add(info.Size())
		}
	}
	return s, nil
}

// Start launches the writer goroutine and the passive poll observer.
func (s *Sink) Start() {
	if s == nil || !s.started.CompareAndSwap(false, true) {
		return
	}
	cliproxyauth.SetQuotaPollObserver(s.ObservePoll)
	go s.run()
}

// HandleUsage implements usage.Plugin. It never blocks: when the queue is full
// the row is dropped and counted.
func (s *Sink) HandleUsage(ctx context.Context, rec usage.Record) {
	if s == nil {
		return
	}
	now := s.now()
	row, observations := buildAttempt(ctx, rec, s.runID, now)
	s.enqueue(item{A: row})
	for _, obs := range observations {
		s.enqueue(item{O: obs})
	}
}

// ObservePoll receives passive usage poll results. It never blocks.
func (s *Sink) ObservePoll(obs cliproxyauth.QuotaPollObservation) {
	if s == nil {
		return
	}
	for _, row := range pollObservations(obs, s.runID, s.now()) {
		s.enqueue(item{O: row})
	}
}

func (s *Sink) enqueue(it item) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.queueDropped.Add(1)
		return
	}
	select {
	case s.ch <- it:
	default:
		s.queueDropped.Add(1)
	}
}

// Close stops intake, flushes queued rows to the database or the spool, and
// writes a 'stop' heartbeat. It returns false when ctx ends first; then rows
// still in memory can be lost and the run has no 'stop' heartbeat.
func (s *Sink) Close(ctx context.Context) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	wasClosed := s.closed
	s.closed = true
	s.mu.Unlock()
	if wasClosed {
		return true
	}
	cliproxyauth.SetQuotaPollObserver(nil)
	ok := true
	if s.started.Load() {
		close(s.stop)
		select {
		case <-s.done:
		case <-ctx.Done():
			ok = false
		}
	}
	if ok && s.db != nil {
		_ = s.db.Close()
	}
	return ok
}

func (s *Sink) heartbeat(kind string) item {
	s.seq++
	return item{H: &heartbeatRow{
		RunID: s.runID, Seq: s.seq, At: s.now(), Kind: kind,
		QueueDropped: s.queueDropped.Load(), SpoolDropped: s.spoolDropped.Load(),
		Spooled: s.spooled.Load(), SpoolBytes: s.spoolBytes.Load(), WriteErrors: s.writeErrors.Load(),
	}}
}

func (s *Sink) run() {
	defer close(s.done)
	batch := []item{s.heartbeat("start")}
	s.replay()
	flushT := time.NewTicker(flushEvery)
	beatT := time.NewTicker(heartbeatEvery)
	replayT := time.NewTicker(replayEvery)
	defer flushT.Stop()
	defer beatT.Stop()
	defer replayT.Stop()
	for {
		select {
		case it := <-s.ch:
			batch = append(batch, it)
			if len(batch) >= batchSize {
				batch = s.flush(batch)
			}
		case <-flushT.C:
			batch = s.flush(batch)
		case <-beatT.C:
			batch = append(batch, s.heartbeat("beat"))
		case <-replayT.C:
			if s.spoolBytes.Load() > 0 {
				s.replay()
			}
		case <-s.stop:
			for {
				select {
				case it := <-s.ch:
					batch = append(batch, it)
					if len(batch) >= batchSize {
						batch = s.flush(batch)
					}
					continue
				default:
				}
				break
			}
			batch = append(batch, s.heartbeat("stop"))
			s.flush(batch)
			return
		}
	}
}

// flush writes the batch to the database, or to the spool on failure.
func (s *Sink) flush(batch []item) []item {
	if len(batch) == 0 {
		return batch
	}
	rejected, errWrite := s.writeIsolated(batch)
	if errWrite != nil {
		s.writeErrors.Add(1)
		log.Warnf("usage analytics: database write failed, spooling %d rows: %s", len(batch), safeErr(errWrite))
		s.spool(batch)
	} else {
		s.quarantine(rejected)
	}
	return batch[:0]
}

// writeIsolated writes items in one transaction. When the database rejects the
// batch for bad data, it writes each row alone so valid rows still commit, and
// returns the rejected rows. On any other error it returns the error; nothing
// is rejected then, and a later retry is idempotent.
func (s *Sink) writeIsolated(items []item) ([]item, error) {
	errWrite := s.writeOnce(items)
	if errWrite == nil || !isDataError(errWrite) {
		return nil, errWrite
	}
	var rejected []item
	for i := range items {
		errRow := s.writeOnce(items[i : i+1])
		switch {
		case errRow == nil:
		case isDataError(errRow):
			rejected = append(rejected, items[i])
		default:
			return nil, errRow
		}
	}
	return rejected, nil
}

func (s *Sink) writeOnce(items []item) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	return s.write(ctx, items)
}

// quarantine keeps rows the database rejects for good in a .bad file for
// recovery. They count as dropped, and the file counts toward the spool cap.
func (s *Sink) quarantine(rows []item) {
	if len(rows) == 0 {
		return
	}
	s.spoolDropped.Add(int64(len(rows)))
	log.Warnf("usage analytics: database rejected %d rows; kept in a .bad spool file", len(rows))
	s.writeSegment(rows, spoolSuffix+".bad")
}

// safeErr reduces an error to text that cannot carry DSN parts or row data.
func safeErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var code interface{ SQLState() string }
	if errors.As(err, &code) {
		return "sqlstate " + code.SQLState()
	}
	return fmt.Sprintf("%T", err)
}

// isDataError reports SQLSTATE classes 22 (data) and 23 (constraint): retrying
// the same rows cannot succeed.
func isDataError(err error) bool {
	var code interface{ SQLState() string }
	if !errors.As(err, &code) {
		return false
	}
	state := code.SQLState()
	return strings.HasPrefix(state, "22") || strings.HasPrefix(state, "23")
}

// spool appends one segment file: write tmp, fsync, rename, fsync directory.
func (s *Sink) spool(batch []item) {
	if s.writeSegment(batch, spoolSuffix) {
		s.spooled.Add(int64(len(batch)))
	} else {
		s.spoolDropped.Add(int64(len(batch)))
	}
}

// writeSegment writes rows to a new spool file within the cap.
func (s *Sink) writeSegment(batch []item, suffix string) bool {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range batch {
		if errEnc := enc.Encode(batch[i]); errEnc != nil {
			return false
		}
	}
	size := int64(buf.Len())
	if size == 0 {
		return false
	}
	if s.spoolBytes.Load()+size > s.spoolMax {
		log.Warnf("usage analytics: spool full, dropped %d rows", len(batch))
		return false
	}
	s.fileSeq++
	name := fmt.Sprintf("%020d-%06d%s", s.now().UnixNano(), s.fileSeq, suffix)
	if errWrite := writeFileSync(s.spoolDir, name, buf.Bytes()); errWrite != nil {
		log.Warnf("usage analytics: spool write failed, dropped %d rows: %v", len(batch), errWrite)
		return false
	}
	s.spoolBytes.Add(size)
	return true
}

func writeFileSync(dir, name string, data []byte) error {
	tmp, errCreate := os.CreateTemp(dir, ".tmp-*")
	if errCreate != nil {
		return errCreate
	}
	tmpName := tmp.Name()
	_, errWrite := tmp.Write(data)
	if errWrite == nil {
		errWrite = tmp.Sync()
	}
	if errClose := tmp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpName, filepath.Join(dir, name))
	}
	if errWrite != nil {
		_ = os.Remove(tmpName)
		return errWrite
	}
	d, errOpen := os.Open(dir)
	if errOpen != nil {
		return errOpen
	}
	errSync := d.Sync()
	if errClose := d.Close(); errSync == nil {
		errSync = errClose
	}
	return errSync
}

func (s *Sink) spoolFiles() ([]string, error) {
	files, errGlob := filepath.Glob(filepath.Join(s.spoolDir, "*"+spoolSuffix))
	if errGlob != nil {
		return nil, errGlob
	}
	sort.Strings(files)
	return files, nil
}

// replay inserts spooled segments in order. A segment is deleted only after its
// insert commits; a crash in between replays it again, and the primary keys
// make that a no-op.
func (s *Sink) replay() {
	files, errList := s.spoolFiles()
	if errList != nil {
		return
	}
	for _, f := range files {
		data, errRead := os.ReadFile(f)
		if errRead != nil {
			return
		}
		var items []item
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(nil, 4<<20)
		for sc.Scan() {
			var it item
			if json.Unmarshal(sc.Bytes(), &it) == nil && (it.A != nil || it.O != nil || it.H != nil) {
				items = append(items, it)
			} else {
				s.spoolDropped.Add(1)
			}
		}
		if len(items) > 0 {
			rejected, errWrite := s.writeIsolated(items)
			if errWrite != nil {
				return
			}
			s.quarantine(rejected)
		}
		if errRemove := os.Remove(f); errRemove != nil {
			return
		}
		s.spoolBytes.Add(-int64(len(data)))
	}
}

// insertItems writes a batch in one transaction. ON CONFLICT DO NOTHING makes
// replay idempotent.
func insertItems(ctx context.Context, db *sql.DB, items []item) error {
	tx, errBegin := db.BeginTx(ctx, nil)
	if errBegin != nil {
		return errBegin
	}
	defer func() { _ = tx.Rollback() }()
	for i := range items {
		var errExec error
		switch it := items[i]; {
		case it.A != nil:
			_, errExec = tx.ExecContext(ctx, insertSQL("attempts", attemptColumns, len(it.A.args())), it.A.args()...)
		case it.O != nil:
			_, errExec = tx.ExecContext(ctx, insertSQL("quota_observations", observationColumns, len(it.O.args())), it.O.args()...)
		case it.H != nil:
			_, errExec = tx.ExecContext(ctx, insertSQL("collector_heartbeats", heartbeatColumns, len(it.H.args())), it.H.args()...)
		}
		if errExec != nil {
			return errExec
		}
	}
	return tx.Commit()
}

// insertSQL has no conflict target: a target needs SELECT privilege, and the
// primary key is the only unique constraint on each table.
func insertSQL(table, columns string, n int) string {
	params := make([]string, n)
	for i := range params {
		params[i] = fmt.Sprintf("$%d", i+1)
	}
	return "INSERT INTO subscription_analytics." + table + " (" + columns + ") VALUES (" +
		strings.Join(params, ", ") + ") ON CONFLICT DO NOTHING"
}
