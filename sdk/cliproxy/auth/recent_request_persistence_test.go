package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecentRequestStoreRestartRecoveryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard-traffic.jsonl")
	store := NewRecentRequestStore(path)
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-1", Provider: "codex", Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errSet := manager.SetRecentRequestStore(store); errSet != nil {
		t.Fatal(errSet)
	}
	manager.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt", Success: true})
	manager.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt", Success: false})

	restarted := NewManager(nil, nil, nil)
	if _, errRegister := restarted.Register(context.Background(), &Auth{ID: "auth-1", Provider: "codex", Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errSet := restarted.SetRecentRequestStore(store); errSet != nil {
		t.Fatal(errSet)
	}
	assertRecentRequestTotals(t, restarted, 1, 1)
	if errSet := restarted.SetRecentRequestStore(store); errSet != nil {
		t.Fatal(errSet)
	}
	assertRecentRequestTotals(t, restarted, 1, 1)
}

func TestRecentRequestStoreLoadsLegacyHistoryFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard-traffic.jsonl")
	legacy := `{"auth_id":"auth-1","provider":"codex","unix":1700000000,"success":true}` + "\n"
	if errWrite := os.WriteFile(path, []byte(legacy), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	records, errLoad := NewRecentRequestStore(path).Load()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(records) != 1 || records[0].AuthID != "auth-1" || records[0].Time.Unix() != 1700000000 || !records[0].Success {
		t.Fatalf("records = %+v", records)
	}
}

func TestRecentRequestStoreIgnoresCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard-traffic.jsonl")
	store := NewRecentRequestStore(path)
	if errAppend := store.Append(RecentRequestRecord{AuthID: "auth-1", Provider: "codex", Time: time.Now(), Success: true}); errAppend != nil {
		t.Fatal(errAppend)
	}
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	_, _ = file.WriteString(`{"auth_id":`)
	_ = file.Close()

	records, errLoad := store.Load()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(records) != 1 || records[0].AuthID != "auth-1" || !records[0].Success {
		t.Fatalf("records = %+v", records)
	}
}

func assertRecentRequestTotals(t *testing.T, manager *Manager, success, failed int64) {
	t.Helper()
	auth, ok := manager.GetByID("auth-1")
	if !ok || auth == nil {
		t.Fatal("missing auth")
	}
	if auth.Success != success || auth.Failed != failed {
		t.Fatalf("totals = %d/%d, want %d/%d", auth.Success, auth.Failed, success, failed)
	}
	var bucketSuccess, bucketFailed int64
	for _, bucket := range auth.RecentRequestsSnapshot(time.Now()) {
		bucketSuccess += bucket.Success
		bucketFailed += bucket.Failed
	}
	if bucketSuccess != success || bucketFailed != failed {
		t.Fatalf("bucket totals = %d/%d, want %d/%d", bucketSuccess, bucketFailed, success, failed)
	}
}
