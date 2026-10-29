package usageanalytics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// fakeDB keeps rows by primary key, like ON CONFLICT DO NOTHING.
type fakeDB struct {
	mu   sync.Mutex
	fail error
	// rejectProvider makes any batch holding an attempt of that provider fail
	// with a data error, like PostgreSQL rejecting one bad row.
	rejectProvider string
	rows           map[string]item
}

func newFakeDB() *fakeDB { return &fakeDB{rows: map[string]item{}} }

func (f *fakeDB) write(_ context.Context, items []item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	for _, it := range items {
		if f.rejectProvider != "" && it.A != nil && it.A.Provider == f.rejectProvider {
			return &pgconn.PgError{Code: "22P05"}
		}
	}
	for _, it := range items {
		switch {
		case it.A != nil:
			f.rows["a:"+it.A.EventID] = it
		case it.O != nil:
			f.rows["o:"+it.O.ObservationID] = it
		case it.H != nil:
			f.rows["h:"+it.H.RunID+":"+it.H.Kind+":"+time.Duration(it.H.Seq).String()] = it
		}
	}
	return nil
}

func (f *fakeDB) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.rows {
		if k[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func closeSink(t *testing.T, s *Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.Close(ctx) {
		t.Fatal("close did not finish")
	}
}

func TestHandleUsageNeverBlocksAndCountsDrops(t *testing.T) {
	s, errNew := newSink(t.TempDir(), newFakeDB().write)
	if errNew != nil {
		t.Fatal(errNew)
	}
	// The writer is not started, so the queue fills up.
	for i := 0; i < queueSize+10; i++ {
		s.HandleUsage(context.Background(), usage.Record{Provider: "p"})
	}
	if got := s.queueDropped.Load(); got != 10 {
		t.Fatalf("dropped %d, want 10", got)
	}
}

func TestCloseFlushesAndWritesStopHeartbeat(t *testing.T) {
	db := newFakeDB()
	s, _ := newSink(t.TempDir(), db.write)
	s.Start()
	for i := 0; i < 3; i++ {
		s.HandleUsage(context.Background(), usage.Record{Provider: "p"})
	}
	closeSink(t, s)
	if db.count("a:") != 3 || db.count("h:") != 2 {
		t.Fatalf("rows %d heartbeats %d", db.count("a:"), db.count("h:"))
	}
	s.HandleUsage(context.Background(), usage.Record{Provider: "p"})
	if s.queueDropped.Load() != 1 {
		t.Fatal("record after close was not counted as dropped")
	}
}

func TestOutageSpoolsAndReplayIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	down := newFakeDB()
	down.fail = errors.New("connection refused")
	s, _ := newSink(dir, down.write)
	s.Start()
	for i := 0; i < 5; i++ {
		s.HandleUsage(context.Background(), usage.Record{Provider: "p"})
	}
	closeSink(t, s)
	files, _ := filepath.Glob(filepath.Join(dir, "*"+spoolSuffix))
	if len(files) == 0 || s.spooled.Load() != 7 {
		t.Fatalf("spool files %d spooled %d", len(files), s.spooled.Load())
	}
	saved, _ := os.ReadFile(files[0])

	up := newFakeDB()
	s2, _ := newSink(dir, up.write)
	if s2.spoolBytes.Load() == 0 {
		t.Fatal("existing spool size not counted")
	}
	s2.replay()
	if up.count("a:") != 5 {
		t.Fatalf("replayed attempts %d", up.count("a:"))
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*"+spoolSuffix)); len(left) != 0 || s2.spoolBytes.Load() != 0 {
		t.Fatal("spool not cleared after commit")
	}
	// A crash after commit but before delete replays the same file again.
	if errWrite := os.WriteFile(files[0], saved, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	s2.replay()
	if up.count("a:") != 5 {
		t.Fatalf("replay duplicated rows: %d", up.count("a:"))
	}
}

func TestSpoolCapDropsAndCounts(t *testing.T) {
	db := newFakeDB()
	db.fail = errors.New("down")
	s, _ := newSink(t.TempDir(), db.write)
	s.spoolMax = 10
	s.spool([]item{{H: &heartbeatRow{RunID: "r", Kind: "beat"}}})
	if s.spoolDropped.Load() != 1 || s.spooled.Load() != 0 {
		t.Fatalf("dropped %d spooled %d", s.spoolDropped.Load(), s.spooled.Load())
	}
}

func TestReplaySetsAsideRejectedSegment(t *testing.T) {
	dir := t.TempDir()
	db := newFakeDB()
	s, _ := newSink(dir, db.write)
	s.spool([]item{{H: &heartbeatRow{RunID: "r", Seq: 1, Kind: "beat"}}})
	size := s.spoolBytes.Load()
	db.fail = &pgconn.PgError{Code: "23514"}
	s.replay()
	if bad, _ := filepath.Glob(filepath.Join(dir, "*.bad")); len(bad) != 1 {
		t.Fatal("rejected segment not set aside")
	}
	// The kept segment still counts toward the cap, and its rows count as dropped.
	if s.spoolBytes.Load() != size || s.spoolDropped.Load() != 1 {
		t.Fatalf("spool bytes %d (want %d) dropped %d", s.spoolBytes.Load(), size, s.spoolDropped.Load())
	}
	// A restart counts it again, and the cap applies to it.
	s2, _ := newSink(dir, db.write)
	if s2.spoolBytes.Load() != size {
		t.Fatalf("restart spool bytes %d, want %d", s2.spoolBytes.Load(), size)
	}
	s2.spoolMax = size
	s2.spool([]item{{H: &heartbeatRow{RunID: "r", Seq: 2, Kind: "beat"}}})
	if s2.spoolDropped.Load() != 1 || s2.spooled.Load() != 0 {
		t.Fatal(".bad segment did not count toward the spool cap")
	}
	if safeErr(db.fail) != "sqlstate 23514" {
		t.Fatalf("safeErr %q", safeErr(db.fail))
	}
}

func TestPollObserverIsPassive(t *testing.T) {
	db := newFakeDB()
	s, _ := newSink(t.TempDir(), db.write)
	s.Start()
	s.ObservePoll(cliproxyauth.QuotaPollObservation{AuthID: "acct", Provider: "claude", ObservedAt: time.Now(),
		Windows: []cliproxyauth.QuotaPollWindow{{Key: "weekly", UsedPercent: 12.5}, {Key: "short", UsedPercent: 5000}}})
	closeSink(t, s)
	if db.count("o:") != 1 {
		t.Fatalf("poll observations %d", db.count("o:"))
	}
}

// Delivery through the usage manager does not depend on usage-statistics-enabled.
func TestDeliveryWithUsageStatisticsDisabled(t *testing.T) {
	redisqueue.SetUsageStatisticsEnabled(false)
	db := newFakeDB()
	s, _ := newSink(t.TempDir(), db.write)
	s.Start()
	m := usage.NewManager(16)
	m.Register(s)
	m.Publish(context.Background(), usage.Record{Provider: "p"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !m.StopAndWait(ctx) {
		t.Fatal("manager did not drain")
	}
	closeSink(t, s)
	if db.count("a:") != 1 {
		t.Fatalf("attempts %d", db.count("a:"))
	}
}

func TestBadRowDoesNotBlockValidSiblings(t *testing.T) {
	dir := t.TempDir()
	db := newFakeDB()
	db.rejectProvider = "bad"
	s, _ := newSink(dir, db.write)
	row := func(provider string) item { return item{A: &attemptRow{EventID: provider + "-id", Provider: provider}} }

	s.flush([]item{row("ok1"), row("bad"), row("ok2")})
	if db.count("a:") != 2 || s.spoolDropped.Load() != 1 || s.spooled.Load() != 0 {
		t.Fatalf("flush: rows %d dropped %d spooled %d", db.count("a:"), s.spoolDropped.Load(), s.spooled.Load())
	}
	bad, _ := filepath.Glob(filepath.Join(dir, "*.bad"))
	if len(bad) != 1 || s.spoolBytes.Load() == 0 {
		t.Fatal("rejected row not kept in a .bad file")
	}

	// Replay: valid rows of a spooled segment commit; the bad one is set aside.
	s.spool([]item{row("ok3"), row("bad")})
	s.replay()
	if db.count("a:") != 3 || s.spoolDropped.Load() != 2 {
		t.Fatalf("replay: rows %d dropped %d", db.count("a:"), s.spoolDropped.Load())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*"+spoolSuffix)); len(left) != 0 {
		t.Fatal("replayed segment not removed")
	}
	if bad, _ = filepath.Glob(filepath.Join(dir, "*.bad")); len(bad) != 2 {
		t.Fatalf(".bad files %d", len(bad))
	}
}

func TestCleanTextForPostgres(t *testing.T) {
	if got := *optString("a\x00b\xff"); got != "ab\uFFFD" {
		t.Fatalf("cleaned %q", got)
	}
}
