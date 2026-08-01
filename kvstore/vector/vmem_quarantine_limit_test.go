package vector

import (
	"fmt"
	"math"
	"testing"
)

// =============================================================================
// LIMIT относится к ОТОЗВАННЫМ, а не к кандидатам.
//
// 🚨ЧТО БЫЛО СЛОМАНО (измерено 31.07, починено 01.08). Скан набирал первые
// LIMIT ключей источника, и только потом приговор отбрасывал уже отозванные.
// Повторный вызов набирал ТЕХ ЖЕ и возвращал 0 при неотозванном остатке:
//
//	20 фактов, LIMIT 5 → вызов 1: снято 5 | вызов 2: снято 0, в памяти 15
//
// Оператор делал ровно то, что велит docs/COMMANDS.md («take the tail with
// another call»), видел ноль и заключал, что работа закончена. Ноль означал
// две разные вещи — «всё чисто» и «первая партия снята, остальное не тронуто»,
// — и различить их снаружи было нечем.
//
// ⭐ПОЧЕМУ ЭТОГО ТЕСТА НЕ БЫЛО. В мутационном прогоне 30.07 мутация «снята
// проверка идемпотентности отзыва» осталась ЖИВОЙ, и это было замечено с
// пометкой «сценарий не делает повторный отзыв». Пропуск назвали и не
// превратили в работу. Урок: «мутация выжила, потому что сценарий этого не
// делает» — не оправдание мутации, а УКАЗАНИЕ НА НЕДОСТАЮЩИЙ СЦЕНАРИЙ.
//
// ⭐ПОЧЕМУ МАЛО ОТСЕЯТЬ «уже отозван». Бюджет партии навсегда съедает любая
// ПОСТОЯННАЯ причина отказа: чужой scope, факт раньше SINCE, истёкший по TTL.
// Каждая из них воспроизводит ровно тот же симптом на своём предикате,
// поэтому ниже проверяется каждая отдельно.
// =============================================================================

// quarantineLimitStore — n фактов одного источника в одном скоупе.
func quarantineLimitStore(t *testing.T, n int, source, scope string, at int64) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	for i := 0; i < n; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID:     fmt.Sprintf("f%02d", i),
			Scope:  scope,
			Text:   fmt.Sprintf("подсаженный факт номер %d", i),
			Source: source,
		}, at); err != nil {
			t.Fatalf("Remember(%d): %v", i, err)
		}
	}
	return lvs
}

// quarantinedCount — сколько фактов реально несут ось отзыва.
func quarantinedCount(lvs *LeveledVectorStore, n int) int {
	got := 0
	for i := 0; i < n; i++ {
		if q := lvs.vmemQuarantinedAt(fmt.Sprintf("f%02d", i)); !math.IsNaN(q) {
			got++
		}
	}
	return got
}

// TestVMEMQuarantineLimitTakesTail — тот самый сценарий из отчёта: хвост
// берётся повторными вызовами, и ноль наступает ТОЛЬКО когда отзывать нечего.
func TestVMEMQuarantineLimitTakesTail(t *testing.T) {
	const (
		total = 20
		limit = 5
	)
	lvs := quarantineLimitStore(t, total, "bad", "user:a", 1000)
	defer lvs.Close()

	req := QuarantineRequest{Scope: "user:a", Source: "bad", Limit: limit}
	revoked := 0
	calls := 0
	for {
		res, err := lvs.Quarantine(req, 2000)
		if err != nil {
			t.Fatalf("Quarantine (вызов %d): %v", calls+1, err)
		}
		calls++
		if len(res.Docs) == 0 {
			break
		}
		if len(res.Docs) > limit {
			t.Fatalf("вызов %d вернул %d фактов при LIMIT %d", calls, len(res.Docs), limit)
		}
		revoked += len(res.Docs)
		if calls > total+2 {
			t.Fatal("вызовы не сходятся — цикл не завершается")
		}
	}

	if revoked != total {
		t.Errorf("отозвано суммарно %d из %d за %d вызовов — хвост недостижим повторным вызовом",
			revoked, total, calls)
	}
	// ⭐Сверка по СОСТОЯНИЮ, а не по сумме квитанций: квитанции могли бы
	// сойтись и при том, что одни и те же факты отзываются по кругу.
	if got := quarantinedCount(lvs, total); got != total {
		t.Errorf("ось quarantined_at стоит у %d фактов из %d — квитанции сошлись, память нет", got, total)
	}
	// Контроль диагноза: за один вызов всё не должно было сняться, иначе
	// проверка выше прошла бы, не коснувшись LIMIT вовсе.
	if calls < total/limit {
		t.Fatalf("вызовов %d при total=%d limit=%d — LIMIT не применялся, тест ничего не проверил",
			calls, total, limit)
	}
}

// TestVMEMQuarantineLimitNotEatenByForeignScope — факты того же источника в
// ЧУЖОМ скоупе не должны съедать бюджет партии. До правки скан набирал их
// первыми, приговор отбрасывал, и свои факты не отзывались никогда.
func TestVMEMQuarantineLimitNotEatenByForeignScope(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// 10 чужих (user:b) и 3 своих (user:a) от одного источника.
	for i := 0; i < 10; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID: fmt.Sprintf("other%02d", i), Scope: "user:b",
			Text: "чужая память", Source: "bad",
		}, 1000); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID: fmt.Sprintf("mine%02d", i), Scope: "user:a",
			Text: "своя память", Source: "bad",
		}, 1000); err != nil {
			t.Fatal(err)
		}
	}

	res, err := lvs.Quarantine(QuarantineRequest{
		Scope: "user:a", Source: "bad", Limit: 5,
	}, 2000)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if len(res.Docs) != 3 {
		t.Errorf("отозвано %d из 3 своих — бюджет партии съеден фактами чужого скоупа", len(res.Docs))
	}
	// Парный контроль: чужой скоуп не тронут (иначе «3» могло бы набраться
	// из чужих фактов, и тест был бы зелёным по неверной причине).
	for i := 0; i < 10; i++ {
		if q := lvs.vmemQuarantinedAt(fmt.Sprintf("other%02d", i)); !math.IsNaN(q) {
			t.Fatalf("отозван факт чужого скоупа other%02d — карантин вышел за границу памяти", i)
		}
	}
}

// TestVMEMQuarantineLimitNotEatenBySince — то же самое на предикате SINCE:
// факты старше границы отбраковываются навсегда и бюджет тратить не должны.
func TestVMEMQuarantineLimitNotEatenBySince(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// 10 старых (valid_from=1000) и 3 новых (valid_from=5000).
	for i := 0; i < 10; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID: fmt.Sprintf("old%02d", i), Scope: "user:a",
			Text: "старый факт", Source: "bad",
		}, 1000); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID: fmt.Sprintf("new%02d", i), Scope: "user:a",
			Text: "новый факт", Source: "bad",
		}, 5000); err != nil {
			t.Fatal(err)
		}
	}

	res, err := lvs.Quarantine(QuarantineRequest{
		Scope: "user:a", Source: "bad", Since: 4000, Limit: 5,
	}, 6000)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if len(res.Docs) != 3 {
		t.Errorf("отозвано %d из 3 попадающих под SINCE — бюджет съеден фактами старше границы", len(res.Docs))
	}
	for i := 0; i < 10; i++ {
		if q := lvs.vmemQuarantinedAt(fmt.Sprintf("old%02d", i)); !math.IsNaN(q) {
			t.Fatalf("отозван факт старше SINCE (old%02d) — нижняя граница не соблюдена", i)
		}
	}
}

// TestVMEMQuarantineLimitAcrossSegments — тот же хвост, но когда факты уже
// уехали в сегменты: скан по колонке — отдельный код от скана по дельте, и
// дефект мог бы жить только в одном из них.
func TestVMEMQuarantineLimitAcrossSegments(t *testing.T) {
	const (
		total = 20
		limit = 5
	)
	lvs := quarantineLimitStore(t, total, "bad", "user:a", 1000)
	defer lvs.Close()
	lvs.FlushDeltaSync()

	// Контроль размещения: без него тест мог бы проверять дельту повторно.
	if segs := lvs.Stats().SegmentsByLevel; len(segs) == 0 || segs[0] == 0 {
		t.Fatalf("факты не уехали в сегменты (%v) — проверялся бы тот же путь, что выше", segs)
	}

	req := QuarantineRequest{Scope: "user:a", Source: "bad", Limit: limit}
	revoked, calls := 0, 0
	for {
		res, err := lvs.Quarantine(req, 2000)
		if err != nil {
			t.Fatalf("Quarantine (вызов %d): %v", calls+1, err)
		}
		calls++
		if len(res.Docs) == 0 {
			break
		}
		revoked += len(res.Docs)
		if calls > total+2 {
			t.Fatal("вызовы не сходятся — цикл не завершается")
		}
	}
	if revoked != total {
		t.Errorf("из сегментов отозвано %d из %d за %d вызовов", revoked, total, calls)
	}
	if got := quarantinedCount(lvs, total); got != total {
		t.Errorf("ось quarantined_at стоит у %d фактов из %d", got, total)
	}
}
