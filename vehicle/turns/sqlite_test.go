package turns

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func TestSQLiteScopeQueueAcrossInstances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "turns.sqlite")
	firstDB, err := OpenSQLite(file)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := OpenSQLite(file)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	first, second := NewSQLiteStore(firstDB), NewSQLiteStore(secondDB)
	scope := contract.Scope{UserID: "U", SessionID: "S", VehicleID: "V"}

	// A stale ticket left behind by a crashed process does not block this turn.
	if _, err := firstDB.Exec("INSERT INTO voice_scope_queue (scope_key, expires_at) VALUES (?, ?)",
		scopeKey(scope), time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	release, err := first.LockScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	type acquired struct {
		name    string
		release func()
		err     error
	}
	results := make(chan acquired, 2)
	for i, name := range []string{"second", "third"} {
		go func(name string) {
			unlock, err := second.LockScope(ctx, scope)
			results <- acquired{name: name, release: unlock, err: err}
		}(name)
		ticker := time.NewTicker(5 * time.Millisecond)
		for {
			var count int
			if err := firstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM voice_scope_queue WHERE scope_key = ?", scopeKey(scope)).Scan(&count); err != nil {
				ticker.Stop()
				t.Fatal(err)
			}
			if count == i+2 {
				ticker.Stop()
				break
			}
			select {
			case early := <-results:
				ticker.Stop()
				t.Fatalf("%s bypassed its queue: %v", early.name, early.err)
			case <-ticker.C:
			case <-ctx.Done():
				ticker.Stop()
				t.Fatal(ctx.Err())
			}
		}
	}
	other := scope
	other.VehicleID = "other"
	unlockOther, err := second.LockScope(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	unlockOther()
	release()
	for _, want := range []string{"second", "third"} {
		select {
		case got := <-results:
			if got.name != want || got.err != nil {
				t.Fatalf("acquired %s instead of %s: %v", got.name, want, got.err)
			}
			got.release()
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
