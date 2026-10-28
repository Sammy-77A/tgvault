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
