package store

import (
	"path/filepath"
	"testing"
)

func TestTempStoreInMemory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA temp_store`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("temp_store = %d, %v; want 2 (memory)", v, err)
	}
}

func TestClaimNextRetriesOverdueFailuresBeforeThePendingQueue(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root, err := s.AddRoot(t.TempDir(), "R")
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) Item {
		t.Helper()
		if err := s.UpsertSeen(Item{Kind: KindFile, CloudPath: "R/" + name, RootID: root.ID, Rel: name, Size: 1, Mtime: 1}); err != nil {
			t.Fatal(err)
		}
		it, err := s.ItemByPath("R/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	pending := add("pending.jpg")
	recent := add("recent-failure.jpg")
	overdue := add("overdue-failure.jpg")
	// A failure whose retry time came a minute ago, and one that came an hour ago.
	if err := s.FinishItem(recent.ID, StateFailed, "x", 1, now()-60, now()-100); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishItem(overdue.ID, StateFailed, "x", 1, now()-3600, now()-7200); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.ClaimNext(0, 0)
	if err != nil || !ok || got.ID != overdue.ID {
		t.Fatalf("first claim = %+v, %v, %v; want the failure that is an hour overdue", got, ok, err)
	}
	got, ok, err = s.ClaimNext(0, 0)
	if err != nil || !ok || got.ID != pending.ID {
		t.Fatalf("second claim = %+v, %v, %v; want the pending file before a failure that is only a minute overdue", got, ok, err)
	}
	got, ok, err = s.ClaimNext(0, 0)
	if err != nil || !ok || got.ID != recent.ID {
		t.Fatalf("third claim = %+v, %v, %v; want the recent failure last", got, ok, err)
	}
}
