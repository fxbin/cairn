package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func holdReadOnlyOpenLock(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "locked.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE marker (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestOpenReadOnlyWaitsForBriefStartupLock(t *testing.T) {
	path, writer := holdReadOnlyOpenLock(t)
	type openResult struct {
		journal *Journal
		err     error
	}
	done := make(chan openResult, 1)
	go func() {
		journal, err := OpenReadOnlyContext(context.Background(), path)
		done <- openResult{journal, err}
	}()
	select {
	case result := <-done:
		if result.journal != nil {
			_ = result.journal.Close()
		}
		t.Fatalf("read-only open returned before the exclusive lock was released: %v", result.err)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := writer.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := result.journal.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read-only open did not recover after lock release")
	}
}

func TestOpenReadOnlyLockWaitHonorsCancellation(t *testing.T) {
	path, writer := holdReadOnlyOpenLock(t)
	defer writer.Exec("ROLLBACK")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		journal, err := OpenReadOnlyContext(ctx, path)
		if journal != nil {
			_ = journal.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("read-only open returned before cancellation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("lock wait ignored cancellation")
	}
}

func TestOpenReadOnlyRejectsDamagedDatabaseWithoutWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "damaged.db")
	content := []byte("not a sqlite database")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenReadOnlyContext(context.Background(), path)
	if journal != nil {
		defer journal.Close()
		_, err = journal.RunIDs(context.Background())
	}
	if err == nil || !IsStateCorruptError(err) {
		t.Fatalf("damaged database was hidden as a transient lock: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(content) {
		t.Fatal("read-only failure modified damaged database")
	}
}
