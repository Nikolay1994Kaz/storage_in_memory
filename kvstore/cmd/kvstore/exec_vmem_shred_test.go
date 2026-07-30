package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"kvstore/kvstore/internal/keyring"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// Сквозная проверка криптостирания. Смысл всей конструкции в одном
// утверждении: после VMEM.SHRED факт не воскресает НИ ИЗ ЧЕГО — ни из WAL, ни
// из снапшота, ни из отгруженного архива, ни при восстановлении на момент до
// стирания. Проверяется это единственным способом, который что-то доказывает:
// прогоном реплея того самого журнала.

// enableKeyring подменяет пакетные точки (sealValue/activeKeyring) на живой
// кейринг и возвращает его. Восстановление — через t.Cleanup, иначе
// соседние тесты пакета получили бы шифрование, которого не просили.
func enableKeyring(t *testing.T, dir string) *keyring.Keyring {
	t.Helper()
	ring, err := keyring.Open(dir)
	if err != nil {
		t.Fatalf("keyring.Open: %v", err)
	}
	prevSeal, prevRing := sealValue, activeKeyring
	sealValue = func(scope string, v []byte) []byte {
		if _, err := ring.EnsureScope(scope); err != nil {
			t.Fatalf("EnsureScope(%s): %v", scope, err)
		}
		sealed, err := ring.Seal(scope, v)
		if err != nil {
			t.Fatalf("Seal(%s): %v", scope, err)
		}
		return sealed
	}
	activeKeyring = ring
	t.Cleanup(func() {
		sealValue, activeKeyring = prevSeal, prevRing
		ring.Close()
	})
	return ring
}

// replaySealedWAL прогоняет журнал через БОЕВОЙ walApplier — тот самый, что
// зовёт main при восстановлении. Раньше здесь было зеркало applyEntry, и оно
// молча разошлось бы с оригиналом; проверять восстановление копией
// восстановления — значит не проверять его вовсе. Возвращает число записей,
// пропущенных из-за уничтоженного ключа.
func replaySealedWAL(t *testing.T, path string, e *execEnv, ring *keyring.Keyring) int {
	t.Helper()
	_, entries, err := wal.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	applier := &walApplier{
		s: e.s, ttl: e.ttl, vec: e.vec, zsetReg: e.zset, ring: ring,
	}
	for _, entry := range entries {
		applier.apply(entry, false)
	}
	return applier.erasedByShred
}

// archiveWAL закрывает журнал среды и снимает его копию — модель сегмента,
// уже уехавшего в объектное хранилище: он существует независимо от того, что
// произойдёт со стором дальше.
//
// ⚠Именно Close, а не Sync. BatchWAL пишет асинхронно, и Sync синхронизирует
// ФАЙЛ, не дожидаясь, пока записи дойдут из канала батчера. Первая версия
// теста снимала архив по Sync — и часть записей в него не попадала, отчего
// проверка «стёртый факт не воскрес» проходила по НЕВЕРНОЙ причине: факта не
// было в архиве вовсе. Поймал это соседний факт, который обязан был выжить.
func archiveWAL(t *testing.T, e *execEnv) string {
	t.Helper()
	if err := e.bw.Close(); err != nil {
		t.Fatalf("wal close: %v", err)
	}
	src := filepath.Join(e.dir, "t.wal")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "archived.wal")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return dst
}

func recallIDs(t *testing.T, e *execEnv, scope, query string) []string {
	t.Helper()
	v := e.do("VMEM.RECALL", scope, "10", query, "ALL")
	if v.Typ == '-' {
		t.Fatalf("RECALL: %v", v.Str)
	}
	var ids []string
	for i := 0; i+2 < len(v.Array)+1 && i < len(v.Array); i += 3 {
		ids = append(ids, v.Array[i].Str)
	}
	return ids
}

// ⭐Главный тест: в журнале нет открытого текста, а после уничтожения ключа
// реплей того же журнала факт не воспроизводит.
func TestVMEMShred_FactDoesNotSurviveReplay(t *testing.T) {
	e1 := newExecEnv(t)
	ring := enableKeyring(t, e1.dir)

	const secret = "Project Aurora cancelled by the steering committee"
	const keep = "Erik prefers morning standups"
	e1.wantBulk(e1.do("VMEM.REMEMBER", "user:dana", "TEXT", secret, "ID", "d1"), "d1")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "user:erik", "TEXT", keep, "ID", "e1"), "e1")

	// ⭐Снимаем КОПИЮ журнала ДО стирания — это точная модель отгруженного
	// архива: он уехал в объектное хранилище раньше, чем поступил запрос на
	// стирание, и удалением его не догнать (шиппер возит поколения и о
	// содержимом не знает). Именно эта копия и решает исход теста.
	archivePath := archiveWAL(t, e1)

	// 1. В архиве нет открытого текста — иначе всё остальное бессмысленно.
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("открытый текст факта лежит в WAL: шифрование на границе персистентности не работает")
	}
	if bytes.Contains(raw, []byte(keep)) {
		t.Fatal("открытый текст чужого факта лежит в WAL")
	}

	// 2. Стираем скоуп dana. Стирание — операция над кейрингом, поэтому
	//    выполняется в живой среде, а архив уже лежит отдельно и неизменен.
	live := newExecEnv(t)
	replaySealedWAL(t, archivePath, live, ring)
	rec := live.do("VMEM.SHRED", "user:dana")
	if rec.Typ == '-' {
		t.Fatalf("SHRED: %v", rec.Str)
	}
	// Десять полей — пять пар. Пятая, chain_seq, это номер звена цепи аудита,
	// по которому квитанцию потом находят. Форма ответа ФИКСИРОВАННАЯ и при
	// выключенной цепи тоже (поле отдаёт "off"): переменная длина заставила бы
	// клиента угадывать, а квитанция — документ.
	if len(rec.Array) != 10 {
		t.Fatalf("квитанция из %d полей, ожидалось 10", len(rec.Array))
	}
	if rec.Array[0].Str != "scope" || rec.Array[1].Str != "user:dana" {
		t.Fatalf("квитанция не про тот скоуп: %v %v", rec.Array[0].Str, rec.Array[1].Str)
	}
	if rec.Array[2].Str != "kek_id" || len(rec.Array[3].Str) != 32 {
		t.Fatalf("квитанция без идентификатора ключа: %v=%q", rec.Array[2].Str, rec.Array[3].Str)
	}
	if ring.HasScope("user:dana") {
		t.Fatal("ключ dana пережил SHRED")
	}
	if !ring.HasScope("user:erik") {
		t.Fatal("SHRED задел чужой скоуп")
	}
	// Уничтожения ключа мало: живой процесс держит факты открытым текстом в
	// памяти, и без их удаления стёртое продолжало бы отдаваться из RECALL до
	// ближайшего перезапуска.
	if ids := recallIDs(t, live, "user:dana", "Aurora"); len(ids) != 0 {
		t.Fatalf("после SHRED факт всё ещё читается из памяти: %v", ids)
	}
	if ids := recallIDs(t, live, "user:erik", "standups"); len(ids) != 1 {
		t.Fatalf("SHRED вынес из памяти чужие факты: %v", ids)
	}

	// 3. Реплей АРХИВА, снятого ДО стирания: стёртый факт не воскресает, чужой
	//    цел. Это и есть то, чего FORGET сделать не может в принципе.
	e2 := newExecEnv(t)
	skipped := replaySealedWAL(t, archivePath, e2, ring)
	if skipped == 0 {
		t.Fatal("реплей не пропустил ни одной записи — ключ не уничтожен?")
	}
	if ids := recallIDs(t, e2, "user:dana", "Aurora"); len(ids) != 0 {
		t.Fatalf("стёртый факт воскрес из журнала: %v", ids)
	}
	if _, ok := e2.s.Get("vmem:d1"); ok {
		t.Fatal("якорь стёртого факта воскрес из журнала")
	}
	if ids := recallIDs(t, e2, "user:erik", "standups"); len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("чужой факт пострадал от стирания: %v", ids)
	}
	if txt, ok := e2.s.Get("vmem:e1"); !ok || string(txt) != keep {
		t.Fatalf("чужой якорь испорчен: %q ok=%v", txt, ok)
	}
}

// Восстановление на момент ДО стирания больше не воскрешает факт — конфликт
// «стирание против PITR», названный в docs/VMEM_DESIGN.md, снят по построению:
// откат отдаёт шифротекст, ключа к нему нет нигде.
func TestVMEMShred_PointInTimeRestoreDoesNotResurrect(t *testing.T) {
	e1 := newExecEnv(t)
	ring := enableKeyring(t, e1.dir)
	const secret = "patient X diagnosis draft"
	e1.wantBulk(e1.do("VMEM.REMEMBER", "user:dana", "TEXT", secret, "ID", "d1"), "d1")
	archivePath := archiveWAL(t, e1)

	// Момент "до стирания" = весь журнал целиком: факт в нём есть.
	before := newExecEnv(t)
	replaySealedWAL(t, archivePath, before, ring)
	if ids := recallIDs(t, before, "user:dana", "diagnosis"); len(ids) != 1 {
		t.Fatalf("предпосылка теста неверна: до стирания факт не читается (%v)", ids)
	}

	if _, _, err := ring.Destroy("user:dana"); err != nil {
		t.Fatal(err)
	}

	after := newExecEnv(t)
	replaySealedWAL(t, archivePath, after, ring)
	if ids := recallIDs(t, after, "user:dana", "diagnosis"); len(ids) != 0 {
		t.Fatalf("откат на момент до стирания воскресил факт: %v", ids)
	}
}

func TestVMEMShred_RefusedWithoutKeyring(t *testing.T) {
	e := newExecEnv(t)
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", "x", "ID", "d1"), "d1")
	e.wantErrPrefix(e.do("VMEM.SHRED", "user:dana"), "ERR encryption at rest is off")
}

// Повторный SHRED не выдаёт вторую квитанцию: расписка об уничтожении того,
// чего уже нет, — документ, утверждающий несделанное.
func TestVMEMShred_SecondCallIssuesNoReceipt(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", "x", "ID", "d1"), "d1")
	if v := e.do("VMEM.SHRED", "user:dana"); v.Typ == '-' {
		t.Fatalf("первый SHRED: %v", v.Str)
	}
	e.wantErrPrefix(e.do("VMEM.SHRED", "user:dana"), "ERR no key for this scope")
}

// Скоуп, писавшийся ДО кейринга, под ключом не находится. Заявить его
// стирание нельзя — это ровно тот капкан, который VMEM.COVERAGE вскрыл для
// провенанса (покрытие оказалось нулевым на реальном сторе).
func TestVMEMShred_RefusesLegacyScopeWithoutKey(t *testing.T) {
	e := newExecEnv(t)
	e.wantBulk(e.do("VMEM.REMEMBER", "user:legacy", "TEXT", "written before the keyring", "ID", "l1"), "l1")
	enableKeyring(t, e.dir) // кейринг появился ПОСЛЕ записи
	e.wantErrPrefix(e.do("VMEM.SHRED", "user:legacy"), "ERR no key for this scope")
}

// ⚠Двухфазная операция: скан и приговор вызываются РАЗДЕЛЬНО, состояние между
// ними меняется. Без такого теста проверялась бы только первая фаза — ровно
// промах, найденный мутацией в VMEM.BACKFILL 27.07.
func TestVMEMShred_PhasesJudgedSeparately(t *testing.T) {
	e := newExecEnv(t)
	lvs := e.vec.(*vector.LeveledVectorStore)
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", "stays in dana", "ID", "d1"), "d1")
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", "moves away", "ID", "d2"), "d2")

	// Фаза скана видит оба факта.
	victims := lvs.CollectScope("user:dana")
	if len(victims) != 2 {
		t.Fatalf("скан нашёл %d фактов, ожидалось 2", len(victims))
	}

	// Между фазами d2 переезжает в другой скоуп (upsert того же id).
	e.wantBulk(e.do("VMEM.REMEMBER", "user:erik", "TEXT", "moves away", "ID", "d2"), "d2")

	// Приговор обязан перепроверить принадлежность и НЕ трогать чужое.
	deleted := lvs.ShredScopeKeys("user:dana", victims)
	if len(deleted) != 1 || deleted[0] != "d1" {
		t.Fatalf("приговор стёр %v, ожидалось только [d1]", deleted)
	}
	if ids := recallIDs(t, e, "user:erik", "moves"); len(ids) != 1 || ids[0] != "d2" {
		t.Fatalf("факт, уехавший в чужой скоуп, стёрт по устаревшему приговору: %v", ids)
	}
}

// ⭐Страж от забытой точки записи. Проверяется не «REMEMBER шифрует», а
// инвариант: НИ ОДНА команда VMEM не должна класть в журнал открытую полезную
// нагрузку. Первая версия интеграции закрывала только REMEMBER — QUARANTINE и
// BACKFILL уехали бы в WAL открытым текстом, и SHRED их бы не покрыл, то есть
// гарантия оказалась бы дырявой ровно там, где её никто не проверяет.
func TestVMEMShred_EveryVMEMWriteIsSealed(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)

	const poison = "Aurora cancelled by an unverified email"
	const plain = "Aurora design review approved"
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", plain, "ID", "d1", "SOURCE", "human"), "d1")
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", poison, "ID", "d2", "SOURCE", "email-agent"), "d2")
	e.wantInt(e.do("VMEM.QUARANTINE", "user:dana", "SOURCE", "email-agent"), 1)
	e.wantInt(e.do("VMEM.BACKFILL", "user:dana", "SOURCE", "imported"), 0)

	archivePath := archiveWAL(t, e)
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{plain, poison} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("открытый текст %q найден в журнале", leak)
		}
	}

	_, entries, err := wal.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		switch entry.Op {
		case wal.OpVSimAddDoc, wal.OpVSimAddDocBatch:
			// Полезная нагрузка дока: вектор, атрибуты и термы текста.
			if !keyring.IsEnvelope(entry.Value) {
				t.Fatalf("%s (key=%s) уехал в журнал без конверта", walOpName(entry.Op), entry.Key)
			}
			checked++
		case wal.OpSet:
			// Якорь VMEM — дословный текст факта.
			if len(entry.Key) > 5 && entry.Key[:5] == "vmem:" && !keyring.IsEnvelope(entry.Value) {
				t.Fatalf("якорь %s уехал в журнал без конверта", entry.Key)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("в журнале нет ни одной VMEM-записи — предпосылка теста неверна")
	}
}
