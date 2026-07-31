package space

import (
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/vault"
)

func TestSetAutoLockReschedulesBackgroundCleanup(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(t.TempDir(), time.Hour, vault.Params{
		Time: 1, MemoryKiB: 8 * 1024, Parallelism: 1,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	created, err := manager.Create(CreateRequest{Password: "password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := manager.SetAutoLock(20 * time.Millisecond); err != nil {
		t.Fatalf("SetAutoLock() error = %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		manager.mu.RLock()
		_, unlocked := manager.sessions[created.Space.ID]
		manager.mu.RUnlock()
		if !unlocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session remained in memory after the updated auto-lock deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
