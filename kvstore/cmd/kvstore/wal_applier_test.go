package main

import (
	"testing"

	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// TestWalApplier_HonorsSnapshotWatermark — гейт «уже в снапшоте» проверяется
// НА ПРИМЕНЕНИИ, а не только таблично.
//
// Дыру нашла мутация: сам shouldSkipVecReplay был покрыт табличным тестом, но
// снятие его ВЫЗОВА в applier не ловил никто. Последствие такого снятия —
// вектор, уже вошедший в бинарный снапшот, накатывается ещё раз из журнала и
// задваивается при каждом рестарте.
func TestWalApplier_HonorsSnapshotWatermark(t *testing.T) {
	e := newExecEnv(t)
	lvs := e.vec.(*vector.LeveledVectorStore)

	applier := &walApplier{
		s: e.s, ttl: e.ttl, vec: e.vec, zsetReg: e.zset,
		graphLoaded: true, vecWatermark: 10,
	}

	blob := vector.SerializeVectorWithDoc([]float32{1, 2, 3, 4}, vector.Attributes{}, nil)

	// LSN ≤ watermark: операция уже отражена в graph_leveled.bin.
	applier.apply(wal.Entry{Op: wal.OpVSimAddDoc, LSN: 5, Key: "covered", Value: blob}, false)
	if _, ok := lvs.Get("covered"); ok {
		t.Error("вектор с LSN ≤ watermark накачен повторно — после рестарта он задвоится")
	}

	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: за watermark'ом запись обязана
	// применяться, иначе «ничего не задвоилось» было бы верно и для applier'а,
	// который просто ничего не делает.
	applier.apply(wal.Entry{Op: wal.OpVSimAddDoc, LSN: 15, Key: "fresh", Value: blob}, false)
	if _, ok := lvs.Get("fresh"); !ok {
		t.Fatal("вектор с LSN > watermark потерян — реплей не применяет ничего")
	}

	// Записи ИЗ СНАПШОТА при загруженном графе пропускаются целиком: их
	// векторный эффект уже в нём, независимо от LSN.
	applier.apply(wal.Entry{Op: wal.OpVSimAddDoc, LSN: 99, Key: "from-snap", Value: blob}, true)
	if _, ok := lvs.Get("from-snap"); ok {
		t.Error("запись из snapshot.wal применена поверх загруженного графа — дубль вектора")
	}

	// Гейт стоит в КАЖДОЙ векторной ветке, и проверять их надо порознь: первый
	// заход покрыл только OpVSimAddDoc, и мутация «снять гейт» в соседней
	// ветке OpVSimAdd прошла мимо. Общий вывод: один случай не проверяет
	// одинаковый по виду код в разных ветках switch.
	plain := vector.SerializeVector([]float32{5, 6, 7, 8})
	applier.apply(wal.Entry{Op: wal.OpVSimAdd, LSN: 5, Key: "covered-plain", Value: plain}, false)
	if _, ok := lvs.Get("covered-plain"); ok {
		t.Error("OpVSimAdd с LSN ≤ watermark накачен повторно — вектор задвоится")
	}
	applier.apply(wal.Entry{Op: wal.OpVSimAdd, LSN: 15, Key: "fresh-plain", Value: plain}, false)
	if _, ok := lvs.Get("fresh-plain"); !ok {
		t.Error("OpVSimAdd с LSN > watermark потерян")
	}

	// То же для OpVSimAddAttrs — третья ветка с тем же гейтом.
	withAttrs := vector.SerializeVectorWithAttrs([]float32{9, 9, 9, 9}, vector.Attributes{})
	applier.apply(wal.Entry{Op: wal.OpVSimAddAttrs, LSN: 5, Key: "covered-attrs", Value: withAttrs}, false)
	if _, ok := lvs.Get("covered-attrs"); ok {
		t.Error("OpVSimAddAttrs с LSN ≤ watermark накачен повторно — вектор задвоится")
	}
	applier.apply(wal.Entry{Op: wal.OpVSimAddAttrs, LSN: 15, Key: "fresh-attrs", Value: withAttrs}, false)
	if _, ok := lvs.Get("fresh-attrs"); !ok {
		t.Error("OpVSimAddAttrs с LSN > watermark потерян")
	}
}
