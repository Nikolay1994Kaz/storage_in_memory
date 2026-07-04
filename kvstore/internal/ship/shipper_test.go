package ship

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"kvstore/kvstore/internal/wal"
)

// newTestShipper — шиппер поверх file://-бэкенда в t.TempDir().
func newTestShipper(t *testing.T, dataDir string) (*Shipper, Remote) {
	t.Helper()
	remote, err := newFileRemote(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	return New(remote, dataDir, Options{RetainManifests: 2}), remote
}

// writeWAL пишет entries в новый WAL-файл и синкает его.
func writeWAL(t *testing.T, path string, entries ...wal.Entry) *wal.WAL {
	t.Helper()
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	return w
}

// replayKV накатывает OpSet/OpDel в map — минимальный оракул состояния.
func replayKV(entries []wal.Entry) map[string]string {
	state := map[string]string{}
	for _, e := range entries {
		switch e.Op {
		case wal.OpSet:
			state[e.Key] = string(e.Value)
		case wal.OpDel:
			delete(state, e.Key)
		}
	}
	return state
}

func restoreAndReplay(t *testing.T, remote Remote, into string) map[string]string {
	t.Helper()
	if _, err := Restore(context.Background(), remote, into); err != nil {
		t.Fatalf("restore: %v", err)
	}
	entries, err := wal.ReadAllWALs(into)
	if err != nil {
		t.Fatalf("read restored WALs: %v", err)
	}
	return replayKV(entries)
}

// TestShipRestore_ActiveWALTail — базовый цикл: хвост активного WAL уезжает
// тиками, restore воспроизводит состояние.
func TestShipRestore_ActiveWALTail(t *testing.T) {
	dataDir := t.TempDir()
	s, remote := newTestShipper(t, dataDir)
	ctx := context.Background()

	w := writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "a", Value: []byte("1")},
		wal.Entry{Op: wal.OpSet, Key: "b", Value: []byte("2")},
	)
	defer w.Close()

	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	// Дописываем хвост — второй тик должен дошипить ТОЛЬКО новые байты.
	if err := w.Write(wal.Entry{Op: wal.OpDel, Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(wal.Entry{Op: wal.OpSet, Key: "c", Value: []byte("3")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	state := restoreAndReplay(t, remote, t.TempDir())
	want := map[string]string{"b": "2", "c": "3"}
	if len(state) != len(want) {
		t.Fatalf("state = %v, want %v", state, want)
	}
	for k, v := range want {
		if state[k] != v {
			t.Fatalf("state[%s] = %q, want %q", k, state[k], v)
		}
	}

	// Хвост уехал двумя кусками (по одному на тик) — offset'ы смежные.
	chunks, err := remote.List(ctx, walDirKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %v, want 2", chunks)
	}
}

// TestShipRestore_CompactionCycle — полный жизненный цикл с компакцией:
// ротация + snapshot + удаление старого WAL; restore после каждой фазы даёт
// корректное состояние, включая НЕвоскрешение удалённого ключа.
func TestShipRestore_CompactionCycle(t *testing.T) {
	dataDir := t.TempDir()
	s, remote := newTestShipper(t, dataDir)
	ctx := context.Background()

	// Фаза 1: пишем и шипим. Ключ doomed будет удалён ПОСЛЕ первого тика —
	// проверяем, что новый снапшот не даст ему воскреснуть на restore.
	w := writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "keep", Value: []byte("v1")},
		wal.Entry{Op: wal.OpSet, Key: "doomed", Value: []byte("x")},
	)
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	if err := w.Write(wal.Entry{Op: wal.OpDel, Key: "doomed"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	// Фаза 2: синхронная «компакция» — как BackgroundCompact, но детерминированно.
	oldPath, err := w.Rotate(filepath.Join(dataDir, "wal_0002.log"))
	if err != nil {
		t.Fatal(err)
	}
	watermark := w.LastLSN()
	state := map[string]string{"keep": "v1"} // состояние после DEL doomed
	sw := wal.NewSnapshotWriter(dataDir)
	err = sw.WriteSnapshot(watermark, func(fn func(op byte, key string, value []byte)) {
		for k, v := range state {
			fn(wal.OpSet, k, []byte(v))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.CleanupOldWALs(dataDir, filepath.Join(dataDir, "wal_0002.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old WAL not cleaned up")
	}

	// Фаза 3: новые записи в новый WAL + два тика (первый может пропустить
	// публикацию из-за смены набора файлов — это штатно).
	if err := w.Write(wal.Entry{Op: wal.OpSet, Key: "after", Value: []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	w.Close()

	got := restoreAndReplay(t, remote, t.TempDir())
	if _, ok := got["doomed"]; ok {
		t.Fatalf("удалённый ключ воскрес после restore: %v", got)
	}
	if got["keep"] != "v1" || got["after"] != "v2" {
		t.Fatalf("state = %v, want keep=v1 after=v2", got)
	}
}

// TestShipRestore_TornTail — оборванная хвостовая запись (недописанный батч на
// момент тика) отбрасывается на restore тем же CRC-путём, что при crash recovery.
func TestShipRestore_TornTail(t *testing.T) {
	dataDir := t.TempDir()
	s, remote := newTestShipper(t, dataDir)
	ctx := context.Background()

	w := writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "ok", Value: []byte("1")},
	)
	w.Close()

	// Рвём хвост: половина заголовка следующей записи.
	f, err := os.OpenFile(filepath.Join(dataDir, "wal_0001.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0xDE, 0xAD, 0xBE}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got := restoreAndReplay(t, remote, t.TempDir())
	if got["ok"] != "1" || len(got) != 1 {
		t.Fatalf("state = %v, want ok=1", got)
	}
}

// TestPublishInvariant_PartialNonActiveWAL — манифест НЕ публикуется, пока
// неактивный WAL недошиплен: частичный старый WAL рядом со свежим снапшотом
// воскрешал бы удалённые в его хвосте ключи.
func TestPublishInvariant_PartialNonActiveWAL(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := newTestShipper(t, dataDir)

	writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "x", Value: []byte("1")},
	).Close()
	writeWAL(t, filepath.Join(dataDir, "wal_0002.log"),
		wal.Entry{Op: wal.OpSet, Key: "y", Value: []byte("2")},
	).Close()

	// Симулируем недошипленный старый файл: доставлено меньше, чем на диске.
	info, err := os.Stat(filepath.Join(dataDir, "wal_0001.log"))
	if err != nil {
		t.Fatal(err)
	}
	s.wals["wal_0001.log"] = &WALFile{Name: "wal_0001.log", Size: info.Size() - 10}
	s.wals["wal_0002.log"] = &WALFile{Name: "wal_0002.log", Size: 0}

	if _, ready := s.buildManifest([]string{"wal_0001.log", "wal_0002.log"}); ready {
		t.Fatal("манифест опубликован с частичным неактивным WAL — нарушен инвариант restore-точки")
	}

	// Дошипленный старый файл — публикация разрешена (активный может быть частичным).
	s.wals["wal_0001.log"].Size = info.Size()
	if _, ready := s.buildManifest([]string{"wal_0001.log", "wal_0002.log"}); !ready {
		t.Fatal("манифест не опубликован при полностью доставленном неактивном WAL")
	}
}

// TestShipper_ResumeAfterRestart — новый экземпляр шиппера продолжает с
// удалённого манифеста: не перезаливает уже доставленное.
func TestShipper_ResumeAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	s, remote := newTestShipper(t, dataDir)
	ctx := context.Background()

	w := writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "a", Value: []byte("1")},
	)
	defer w.Close()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := remote.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	// «Рестарт»: свежий шиппер поверх того же remote и каталога, ничего нового.
	s2 := New(remote, dataDir, Options{RetainManifests: 2})
	if err := s2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := remote.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("рестарт перезалил объекты: было %d, стало %d (%v)", len(before), len(after), after)
	}

	// Новые данные после рестарта дошипливаются от прежнего offset'а.
	if err := w.Write(wal.Entry{Op: wal.OpSet, Key: "b", Value: []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := restoreAndReplay(t, remote, t.TempDir())
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("state = %v, want a=1 b=2", got)
	}
}

// TestRetention — старые генерации и осиротевшие куски удаляются, последняя
// restore-точка остаётся рабочей.
func TestRetention(t *testing.T) {
	dataDir := t.TempDir()
	remote, err := newFileRemote(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(remote, dataDir, Options{RetainManifests: 1})
	ctx := context.Background()

	w := writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "k", Value: []byte("v1")},
	)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	// Две «компакции» подряд — каждая делает старый WAL сиротой.
	for i := 2; i <= 3; i++ {
		if _, err := w.Rotate(filepath.Join(dataDir, fmt.Sprintf("wal_%04d.log", i))); err != nil {
			t.Fatal(err)
		}
		sw := wal.NewSnapshotWriter(dataDir)
		err = sw.WriteSnapshot(w.LastLSN(), func(fn func(op byte, key string, value []byte)) {
			fn(wal.OpSet, "k", fmt.Appendf(nil, "v%d", i))
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := wal.CleanupOldWALs(dataDir, filepath.Join(dataDir, fmt.Sprintf("wal_%04d.log", i))); err != nil {
			t.Fatal(err)
		}
		if err := w.Write(wal.Entry{Op: wal.OpSet, Key: "k", Value: fmt.Appendf(nil, "v%d", i)}); err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		// Два тика: первый видит смену набора файлов, второй публикует+ретеншн.
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	// Куски wal_0001 — сироты (файл выпал из манифестов) и обязаны исчезнуть.
	orphans, err := remote.List(ctx, walDirKey+"wal_0001.log/")
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("осиротевшие куски не удалены ретеншном: %v", orphans)
	}
	manifests, err := remote.List(ctx, manifestDirKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %v, want ровно 1 (retain=1)", manifests)
	}

	got := restoreAndReplay(t, remote, t.TempDir())
	if got["k"] != "v3" {
		t.Fatalf("state = %v, want k=v3", got)
	}
}

// TestRestore_RefusesNonEmptyDir — restore не затирает существующее состояние.
func TestRestore_RefusesNonEmptyDir(t *testing.T) {
	dataDir := t.TempDir()
	s, remote := newTestShipper(t, dataDir)
	writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "a", Value: []byte("1")},
	).Close()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Каталог с существующим WAL — отказ.
	if _, err := Restore(context.Background(), remote, dataDir); err == nil {
		t.Fatal("restore поверх существующего состояния должен отказывать")
	}
}

// TestRestore_EmptyRemote — понятная ошибка, если ещё ничего не доставлено.
func TestRestore_EmptyRemote(t *testing.T) {
	remote, err := newFileRemote(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), remote, t.TempDir()); err == nil {
		t.Fatal("restore из пустого хранилища должен вернуть ошибку")
	}
}

// TestShipSnapshotAndGraph — снапшоты уезжают версионированными объектами и
// восстанавливаются байт-в-байт.
func TestShipSnapshotAndGraph(t *testing.T) {
	dataDir := t.TempDir()
	s, remote := newTestShipper(t, dataDir)
	ctx := context.Background()

	writeWAL(t, filepath.Join(dataDir, "wal_0001.log"),
		wal.Entry{Op: wal.OpSet, Key: "a", Value: []byte("1")},
	).Close()

	sw := wal.NewSnapshotWriter(dataDir)
	err := sw.WriteSnapshot(1, func(fn func(op byte, key string, value []byte)) {
		fn(wal.OpSet, "snapkey", []byte("snapval"))
	})
	if err != nil {
		t.Fatal(err)
	}
	graphBytes := []byte("fake-vector-graph-snapshot")
	if err := os.WriteFile(filepath.Join(dataDir, graphFile), graphBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	into := t.TempDir()
	sum, err := Restore(ctx, remote, into)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Snapshot || !sum.Graph || sum.WALFiles != 1 {
		t.Fatalf("summary = %+v, want snapshot+graph+1 wal", sum)
	}
	restored, err := os.ReadFile(filepath.Join(into, graphFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(graphBytes) {
		t.Fatal("graph_leveled.bin восстановлен не байт-в-байт")
	}
	entries, err := wal.ReadAllWALs(into)
	if err != nil {
		t.Fatal(err)
	}
	state := replayKV(entries)
	if state["snapkey"] != "snapval" || state["a"] != "1" {
		t.Fatalf("state = %v, want snapkey+a", state)
	}

	// Повторный тик без изменений не создаёт новых объектов и генераций.
	before, _ := remote.List(ctx, "")
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := remote.List(ctx, "")
	if len(after) != len(before) {
		t.Fatalf("idempotence: тик без изменений добавил объекты (%d → %d)", len(before), len(after))
	}
}
