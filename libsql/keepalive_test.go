package libsql

import (
	"context"
	"database/sql/driver"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConnector counts Connect calls so a test can tell a keepalive tick from
// application traffic.
type fakeConnector struct {
	connects atomic.Int64
	closed   atomic.Bool
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	f.connects.Add(1)
	return nil, errNoConn // touch() gives up quietly; the count is what matters
}
func (f *fakeConnector) Driver() driver.Driver { return Driver{} }
func (f *fakeConnector) Close() error          { f.closed.Store(true); return nil }

// errNoConn keeps the fake from having to implement a whole driver.Conn.
var errNoConn = driver.ErrBadConn

func eventually(t *testing.T, within time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestKeepAliveIsOptIn: the whole point of the decorator is that a consumer who
// does not ask for it is completely unaffected -- no goroutine, no queries.
func TestKeepAliveIsOptIn(t *testing.T) {
	var cfg config
	inner := &fakeConnector{}

	if got := cfg.withKeepAlive(inner); got != driver.Connector(inner) {
		t.Error("no WithKeepAlive option must leave the connector untouched")
	}

	zero := time.Duration(0)
	cfg.keepaliveInterval = &zero
	if got := cfg.withKeepAlive(inner); got != driver.Connector(inner) {
		t.Error("a non-positive interval must disable the keepalive, not tick forever")
	}

	negative := -time.Second
	cfg.keepaliveInterval = &negative
	if got := cfg.withKeepAlive(inner); got != driver.Connector(inner) {
		t.Error("a negative interval must disable the keepalive")
	}
}

// TestKeepAliveSkippedForFileDatabases: a local file does not suspend, so a
// timer querying local disk is pure waste.
func TestKeepAliveSkippedForFileDatabases(t *testing.T) {
	interval := 25 * time.Millisecond
	cfg := config{keepaliveInterval: &interval}

	file := &fileConnector{url: "file:test.db"}
	if got := cfg.withKeepAlive(file); got != driver.Connector(file) {
		t.Error("file: databases do not suspend and must not get a keepalive")
	}
}

// TestKeepAliveTicksWhenIdle is the core behaviour: after the interval passes
// with no application traffic, the keepalive touches the database itself.
func TestKeepAliveTicksWhenIdle(t *testing.T) {
	inner := &fakeConnector{}
	c := newKeepaliveConnector(inner, 20*time.Millisecond)
	t.Cleanup(func() { _ = c.Close() })

	// The loop starts on first use, so nothing happens until someone connects.
	if inner.connects.Load() != 0 {
		t.Fatal("constructing a connector must not connect")
	}
	_, _ = c.Connect(context.Background())

	eventually(t, 2*time.Second, func() bool { return inner.connects.Load() >= 3 },
		"keepalive did not keep touching an idle database")
}

// TestKeepAliveSkipsWhenTrafficIsFlowing is the adaptive half. A blind ticker
// would query a database that is already warm; on a usage-billed service that
// is waste you pay for.
func TestKeepAliveSkipsWhenTrafficIsFlowing(t *testing.T) {
	inner := &fakeConnector{}
	c := newKeepaliveConnector(inner, 50*time.Millisecond)
	t.Cleanup(func() { _ = c.Close() })

	// Drive steady application traffic for several tick intervals.
	stop := time.After(250 * time.Millisecond)
	var appConnects int64
	for done := false; !done; {
		select {
		case <-stop:
			done = true
		default:
			_, _ = c.Connect(context.Background())
			appConnects++
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Every Connect the fake saw should be one the application made. Allow a
	// small margin for a tick racing the final sleep.
	if extra := inner.connects.Load() - appConnects; extra > 1 {
		t.Errorf("keepalive fired %d times while traffic was flowing; it should have skipped", extra)
	}
}

// TestKeepAliveStopsOnClose: database/sql calls Close on a connector that
// implements io.Closer, so a leaked goroutine here would outlive every DB the
// process opens.
func TestKeepAliveStopsOnClose(t *testing.T) {
	inner := &fakeConnector{}
	c := newKeepaliveConnector(inner, 10*time.Millisecond)

	_, _ = c.Connect(context.Background())
	eventually(t, time.Second, func() bool { return inner.connects.Load() >= 2 },
		"keepalive never started")

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed.Load() {
		t.Error("Close must propagate to a closable inner connector")
	}

	settled := inner.connects.Load()
	time.Sleep(60 * time.Millisecond)
	if got := inner.connects.Load(); got != settled {
		t.Errorf("keepalive kept ticking after Close: %d -> %d", settled, got)
	}

	// Close is called from DB.Close and possibly again by a defer; it must not panic.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestCloseBeforeAnyConnectDoesNotHang pins the bug in the first draft of this
// file: asking a sync.Once whether it has run means running it, which left
// Close waiting on a goroutine that could then never start.
func TestCloseBeforeAnyConnectDoesNotHang(t *testing.T) {
	c := newKeepaliveConnector(&fakeConnector{}, time.Hour)

	done := make(chan struct{})
	go func() { defer close(done); _ = c.Close() }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung when the keepalive loop had never started")
	}
}
