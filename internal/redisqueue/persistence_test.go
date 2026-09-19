package redisqueue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceRestartRecoveryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-queue.jsonl")
	SetEnabled(false)
	SetEnabled(true)
	SetRetentionSeconds(86400)
	t.Cleanup(func() {
		_ = SetPersistencePath("")
		SetEnabled(false)
	})
	if errSet := SetPersistencePath(path); errSet != nil {
		t.Fatalf("SetPersistencePath: %v", errSet)
	}
	Enqueue([]byte(`{"id":1}`))
	Enqueue([]byte(`{"id":2}`))

	if errReload := SetPersistencePath(path); errReload != nil {
		t.Fatalf("first reload: %v", errReload)
	}
	if got := SnapshotNewest(10); len(got) != 2 || string(got[0]) != `{"id":1}` || string(got[1]) != `{"id":2}` {
		t.Fatalf("first snapshot = %q", got)
	}
	if errReload := SetPersistencePath(path); errReload != nil {
		t.Fatalf("second reload: %v", errReload)
	}
	if got := SnapshotNewest(10); len(got) != 2 {
		t.Fatalf("idempotent snapshot len = %d, want 2", len(got))
	}
}

func TestPersistenceIgnoresCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-queue.jsonl")
	SetEnabled(false)
	SetEnabled(true)
	SetRetentionSeconds(86400)
	t.Cleanup(func() {
		_ = SetPersistencePath("")
		SetEnabled(false)
	})
	if errSet := SetPersistencePath(path); errSet != nil {
		t.Fatalf("SetPersistencePath: %v", errSet)
	}
	Enqueue([]byte(`{"id":1}`))
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	_, _ = file.WriteString(`{"enqueued_at":`)
	_ = file.Close()

	if errReload := SetPersistencePath(path); errReload != nil {
		t.Fatalf("reload corrupt tail: %v", errReload)
	}
	got := SnapshotNewest(10)
	if len(got) != 1 || string(got[0]) != `{"id":1}` {
		t.Fatalf("snapshot after corrupt tail = %q", got)
	}
}

func TestPersistencePrunesExpiredRecordsOnReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-queue.jsonl")
	SetEnabled(false)
	SetEnabled(true)
	SetRetentionSeconds(1)
	t.Cleanup(func() {
		_ = SetPersistencePath("")
		SetEnabled(false)
	})
	if errSet := SetPersistencePath(path); errSet != nil {
		t.Fatalf("SetPersistencePath: %v", errSet)
	}
	global.mu.Lock()
	global.items = []queueItem{{enqueuedAt: time.Now().Add(-time.Minute), payload: []byte(`{"old":true}`)}}
	if errRewrite := global.rewritePersistenceLocked(time.Now()); errRewrite != nil {
		global.mu.Unlock()
		t.Fatal(errRewrite)
	}
	global.mu.Unlock()

	if errReload := SetPersistencePath(path); errReload != nil {
		t.Fatal(errReload)
	}
	if got := SnapshotNewest(10); len(got) != 0 {
		t.Fatalf("expired snapshot = %q", got)
	}
}
