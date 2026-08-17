package libsql

import (
	"context"
	"database/sql/driver"
	"sync"
	"sync/atomic"
	"time"
)

// A Turso instance suspends when nothing queries it, and waking costs roughly a
// second and a half. Measured against a Turso database: 49ms after 30s idle,
// 1.83s after 60s. The threshold sits somewhere between the two, so the first
// request after a quiet stretch pays a wake that has nothing to do with the
// query being run.
//
// The usual answer is a keepalive ticking on DB.Ping(). Before this fork, that
// did nothing at all -- the driver's Ping signature did not satisfy
// driver.Pinger, so database/sql never forwarded it and DB.Ping() returned
// without leaving the process. That is fixed, so a Ping-based keepalive now
// works. This is the same thing done better in two respects:
//
//  1. It is ADAPTIVE. A blind ticker queries a database that real traffic is
//     already keeping warm -- pure waste during working hours, and on a
//     usage-billed service it is waste you pay for. This ticks only when
//     nothing else has touched the database.
//  2. The activity signal is taken at the CONNECTOR, so it sees every query the
//     application makes without any call site reporting in.
//
// It is opt-in. A connector built without WithKeepAlive behaves exactly as
// before, spawns no goroutine, and issues no queries of its own.

// keepaliveDefaultStatement is what a tick executes. EXPLAIN QUERY PLAN reports
// SCAN CONSTANT ROW for it -- no table is touched -- so it scans zero rows
// whichever way the server's billing counts them.
const keepaliveDefaultStatement = "SELECT 1"

// WithKeepAlive holds a suspending remote instance awake by issuing a trivial
// statement whenever the connector has been idle for `interval`.
//
// Pick an interval under the server's suspend threshold, which measured between
// 30 and 60 seconds; 25s leaves margin for a late tick. Passing a non-positive
// interval disables the keepalive, so a caller can wire it to a config value
// without branching.
//
// This applies to REMOTE connectors only. A file: database does not suspend, so
// asking for a keepalive on one is quietly ignored rather than burning a
// goroutine to query local disk on a timer.
//
// The keepalive's own goroutine stops when the connector is closed.
// database/sql calls Close on a connector that implements io.Closer, so
// sql.OpenDB(c) followed by db.Close() shuts it down with no extra bookkeeping:
//
//	c, err := libsql.NewConnector(dsn, libsql.WithKeepAlive(25*time.Second))
//	if err != nil { ... }
//	db := sql.OpenDB(c)
//	defer db.Close() // stops the keepalive
func WithKeepAlive(interval time.Duration) Option {
	return option(func(o *config) error {
		o.keepaliveInterval = &interval
		return nil
	})
}

// keepaliveConnector decorates a remote connector with idle tracking and a
// background tick. It deliberately holds no *sql.DB: a tick opens its own
// connection through the inner connector, so the keepalive cannot be starved by
// a saturated pool, and nothing here depends on how the caller wraps us.
type keepaliveConnector struct {
	inner    driver.Connector
	interval time.Duration

	// lastActivity is the unix-nano time of the most recent Connect. Per
	// connector rather than per process: a program may hold several databases,
	// and one busy connector must not keep another's instance from being
	// tickled.
	lastActivity atomic.Int64

	// started guards the loop goroutine. An atomic rather than a sync.Once so
	// Close can ASK whether the loop is running; querying a Once means
	// executing it, which would have let a Close-before-first-Connect
	// permanently prevent the loop from ever starting.
	started  atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func newKeepaliveConnector(inner driver.Connector, interval time.Duration) *keepaliveConnector {
	return &keepaliveConnector{
		inner:    inner,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Connect records activity and delegates.
//
// Connect is a faithful proxy for "a query happened" on this transport: the
// server closes a stream after each statement, so database/sql opens roughly
// one connection per query. It errs safe in any case -- a missed stamp costs
// one unnecessary zero-row query, whereas a false stamp would let the instance
// go cold, and Connect cannot fire without a caller behind it.
//
// The loop starts on first use rather than at construction, so building a
// connector that is never queried never spawns a goroutine.
func (c *keepaliveConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.lastActivity.Store(time.Now().UnixNano())
	if c.started.CompareAndSwap(false, true) {
		go c.loop()
	}
	return c.inner.Connect(ctx)
}

func (c *keepaliveConnector) Driver() driver.Driver { return c.inner.Driver() }

// IdleFor reports how long since this connector last opened a connection, or 0
// before any activity -- so a freshly built connector does not read as idle
// since the epoch.
func (c *keepaliveConnector) IdleFor() time.Duration {
	ns := c.lastActivity.Load()
	if ns == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ns))
}

// Close stops the keepalive and closes the inner connector if it can be closed.
// database/sql invokes this from DB.Close, so callers using sql.OpenDB get the
// goroutine cleaned up for free. Safe to call more than once.
func (c *keepaliveConnector) Close() error {
	c.stopOnce.Do(func() {
		close(c.stop)
		// Only wait for the loop if it was ever started; done is never closed
		// otherwise and this would block forever.
		if c.started.Load() {
			<-c.done
		}
	})
	if closer, ok := c.inner.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func (c *keepaliveConnector) loop() {
	defer close(c.done)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			// Real traffic already kept the instance warm; skip.
			if c.IdleFor() < c.interval {
				continue
			}
			c.touch()
		}
	}
}

// touch runs the keepalive statement on a connection of its own, then closes
// it. Errors are swallowed by design: a keepalive is best-effort, and a
// transient failure here must never surface to an application that did not ask
// for one. The next tick tries again.
func (c *keepaliveConnector) touch() {
	ctx, cancel := context.WithTimeout(context.Background(), c.interval)
	defer cancel()

	conn, err := c.Connect(ctx)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	q, ok := conn.(driver.QueryerContext)
	if !ok {
		return
	}
	rows, err := q.QueryContext(ctx, keepaliveDefaultStatement, nil)
	if err != nil {
		return
	}
	_ = rows.Close()
}
