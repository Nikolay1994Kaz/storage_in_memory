package wal

import (
	"fmt"
	"path/filepath"
	"testing"
)

// ─── WAL.Write (прямой путь) ───

func BenchmarkWAL_Write(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(filepath.Join(dir, "bench.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	entry := Entry{Op: OpSet, Key: "user:12345", Value: []byte("some-value-here-128bytes-padding-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w.Write(entry)
	}
}

// ─── BatchWAL.Write (горячий путь) ───

func BenchmarkBatchWAL_Write(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(filepath.Join(dir, "bench.log"))
	if err != nil {
		b.Fatal(err)
	}

	bw := NewBatchWAL(w)
	defer bw.Close()

	entry := Entry{Op: OpSet, Key: "user:12345", Value: []byte("some-value-here-128bytes-padding-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bw.Write(entry)
	}
}

// ─── flushBatch (кодирование batch) ───

func BenchmarkFlushBatch(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(filepath.Join(dir, "bench.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	bw := &BatchWAL{
		wal:       w,
		encodeBuf: make([]byte, 0, 64*1024),
	}

	batch := make([]Entry, 256)
	for i := range batch {
		batch[i] = Entry{
			Op:    OpSet,
			Key:   fmt.Sprintf("user:%05d", i),
			Value: []byte("some-value-here-128bytes-padding-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"),
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bw.flushBatch(batch)
	}
}

// ─── encodeEntry ───

func BenchmarkEncodeEntry(b *testing.B) {
	entry := Entry{Op: OpSet, Key: "user:12345", Value: []byte("some-value-here-128bytes-padding-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = encodeEntry(entry)
	}
}

// ─── Snapshot Write ───

func BenchmarkSnapshot_Write(b *testing.B) {
	for _, numKeys := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("keys=%d", numKeys), func(b *testing.B) {
			dir := b.TempDir()

			// Готовим данные
			keys := make([]string, numKeys)
			values := make([][]byte, numKeys)
			for i := 0; i < numKeys; i++ {
				keys[i] = fmt.Sprintf("key:%06d", i)
				values[i] = []byte(fmt.Sprintf("value-%06d-padding-xxxxxxxxxxxx", i))
			}

			iterate := func(fn func(key string, value []byte)) {
				for i := 0; i < numKeys; i++ {
					fn(keys[i], values[i])
				}
			}

			sw := NewSnapshotWriter(dir)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sw.WriteSnapshot(iterate)
			}
		})
	}
}

// ─── ReadEntries ───

func BenchmarkReadEntries(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")

	// Создаём файл с 10K записей
	w, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		w.Write(Entry{
			Op:    OpSet,
			Key:   fmt.Sprintf("key:%06d", i),
			Value: []byte(fmt.Sprintf("value-%06d", i)),
		})
	}
	w.Sync()
	w.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ReadEntries(path)
	}
}
