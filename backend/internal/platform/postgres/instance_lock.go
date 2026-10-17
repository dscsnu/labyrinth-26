package postgres

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// instanceLockID is the advisory lock key held by the running backend. It
// differs from migrationLockID.
const instanceLockID = 727_2027

// ErrAnotherInstance means another backend already holds the instance lock on
// this database.
var ErrAnotherInstance = errors.New("another backend instance is already running against this database")

// ErrInstanceLockLost means the connection holding the instance lock failed
// or the lock is no longer held (e.g. Postgres restarted), so another
// instance could now start. The holder must stop.
var ErrInstanceLockLost = errors.New("lost the instance lock")

// InstanceLock is the held instance lock. Its connection is checked every
// interval; Lost is closed if the lock is gone.
type InstanceLock struct {
	conn     *pgxpool.Conn
	lost     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// AcquireInstanceLock takes a session-level advisory lock on one dedicated
// connection, held until Release. Game state lives in this process's memory,
// so a second instance would serve its own diverging copy; it must refuse to
// start (ErrAnotherInstance). Every check interval (config.toml
// storage.instance_lock_check) it verifies the lock is still held, and closes
// Lost if not.
func (db *DB) AcquireInstanceLock(ctx context.Context, check time.Duration) (*InstanceLock, error) {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", instanceLockID).Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, ErrAnotherInstance
	}
	l := &InstanceLock{conn: conn, lost: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	go l.watch(check)
	return l, nil
}

// Lost is closed when the lock is no longer held.
func (l *InstanceLock) Lost() <-chan struct{} { return l.lost }

// watch checks every interval that this session still holds the lock, until
// Release or a failed check.
func (l *InstanceLock) watch(interval time.Duration) {
	defer close(l.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if !l.held(interval) {
				close(l.lost)
				return
			}
		}
	}
}

// held reports whether this connection still holds the lock, within timeout.
// A single-bigint advisory lock shows in pg_locks as classid 0 (high bits),
// objid (low bits), objsubid 1.
func (l *InstanceLock) held(timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var ok bool
	err := l.conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND classid = 0 AND objid = $1 AND objsubid = 1
		  AND pid = pg_backend_pid() AND granted)`, instanceLockID).Scan(&ok)
	return err == nil && ok
}

// Release stops watching, unlocks and returns the connection. Safe to call
// more than once.
func (l *InstanceLock) Release() {
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
		_, _ = l.conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", instanceLockID)
		l.conn.Release()
	})
}
