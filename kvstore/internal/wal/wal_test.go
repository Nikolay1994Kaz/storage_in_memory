package wal

import (
	"path/filepath"
	"testing"
)

func tempWAL(t *testing.T) (*WAL, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_0001.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open WAL: %v", dir)
	}
	return w, dir
}

func TestWAL_WriteAndRead(t *testing.T) {
	w, dir := tempWAL(t)
	entries := []Entry{
		{Op: OpSet, Key: "user:1", Value: []byte("Alice")},
		{Op: OpSet, Key: "user:2", Value: []byte("Bob")},
		{Op: OpDel, Key: "user:1", Value: nil},
	}
	for _, e := range entries {
		if err := w.Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	w.Close()
	got, err := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d entries, want 3", len(got))
	}
	if got[0].Op != OpSet || got[0].Key != "user:1" || string(got[0].Value) != "Alice" {
		t.Fatalf("entry 0: %+v", got[0])
	}
	if got[1].Key != "user:2" || string(got[1].Value) != "Bob" {
		t.Fatalf("entry 1: %+v", got[1])
	}
	if got[2].Op != OpDel || got[2].Key != "user:1" {
		t.Fatalf("entry 2: %+v", got[2])
	}
}
