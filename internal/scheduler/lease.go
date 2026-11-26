package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

const defaultLeaseTTL = 2 * time.Minute

// Lease represents an exclusive owner lock on a WorkItem or Run.
type Lease struct {
	db       *sql.DB
	leaseKey string
	runID    string
	stopCh   chan struct{}
}

// AcquireLease claims leaseKey in workflow.db, taking over expired or dead-PID leases
// and failing if another live local process currently owns the lease.
func AcquireLease(ctx context.Context, db *sql.DB, leaseKey, runID string) (*Lease, error) {
	now := time.Now().UTC()
	host, _ := os.Hostname()
	pid := os.Getpid()
	expiresAt := now.Add(defaultLeaseTTL).Format(time.RFC3339Nano)
	nowISO := now.Format(time.RFC3339Nano)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingRun, existingHost, existingExpires string
	var existingPID int
	err = tx.QueryRowContext(ctx, `
		SELECT run_id, owner_pid, hostname, expires_at FROM run_leases WHERE lease_key = ?
	`, leaseKey).Scan(&existingRun, &existingPID, &existingHost, &existingExpires)

	if err == nil {
		expTime, parseErr := time.Parse(time.RFC3339Nano, existingExpires)
		notExpired := parseErr == nil && expTime.After(now)
		sameHost := existingHost == host
		if notExpired && existingRun != runID && (!sameHost || isPIDAlive(existingPID)) {
			return nil, fmt.Errorf("lease %s is already held by run %s (pid=%d, host=%s)", leaseKey, existingRun, existingPID, existingHost)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_leases WHERE lease_key = ?`, leaseKey); err != nil {
			return nil, err
		}
	} else if err != sql.ErrNoRows {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_leases (lease_key, run_id, owner_pid, hostname, acquired_at, heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, leaseKey, runID, pid, host, nowISO, nowISO, expiresAt); err != nil {
		return nil, fmt.Errorf("acquire lease %s: %w", leaseKey, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	lease := &Lease{
		db:       db,
		leaseKey: leaseKey,
		runID:    runID,
		stopCh:   make(chan struct{}),
	}
	go lease.heartbeatLoop()
	return lease, nil
}

func (l *Lease) heartbeatLoop() {
	ticker := time.NewTicker(defaultLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			now := time.Now().UTC()
			_, _ = l.db.Exec(`
				UPDATE run_leases SET heartbeat_at = ?, expires_at = ?
				WHERE lease_key = ? AND run_id = ?
			`, now.Format(time.RFC3339Nano), now.Add(defaultLeaseTTL).Format(time.RFC3339Nano), l.leaseKey, l.runID)
		}
	}
}

// Release stops heartbeating and deletes the lease owned by this run.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	select {
	case <-l.stopCh:
	default:
		close(l.stopCh)
	}
	_, _ = l.db.Exec(`DELETE FROM run_leases WHERE lease_key = ? AND run_id = ?`, l.leaseKey, l.runID)
}

func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
