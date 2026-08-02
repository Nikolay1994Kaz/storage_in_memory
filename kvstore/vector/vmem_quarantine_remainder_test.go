package vector

import "testing"

// =============================================================================
// ОСТАТОК ПО ИСТОЧНИКУ: что предикат НЕ ДОСТАЛ.
//
// ⭐ЗАЧЕМ ЭТО ОТДЕЛЬНОЕ ИЗМЕРЕНИЕ. Число отозванных не равно полноте лечения.
// Замер границ отзыва (scripts/revocation_limits.py, случай L3) показал так:
// окно вычисляется из valid_from ЗАМЕЧЕННОЙ подсадки — больше оператор ничего
// не знает, — а подсаженное раньше уходит из-под окна. Карантин отрабатывает
// честно, снимает ровно одно и отвечает бодрым «отозвано 1». Две пережившие
// лечение лжи не названы ничем: компромисс цена↔полнота был МОЛЧАЛИВЫМ.
// Считать его умел скрипт рядом с движком, то есть измерение жило в харнессе,
// а продукт на вопрос «а всё ли снято» отвечал молчанием.
//
// 🚨ГЛАВНОЕ, ЧТО ЗДЕСЬ СУДИТСЯ, — не наличие полей, а ЧЕМ они посчитаны.
// Остаток обязан меряться полным проходом по состоянию ПОСЛЕ приговора.
// Соблазнительная дешёвая альтернатива — досчитать его по ходу скана
// кандидатов — неверна ровно в том случае, ради которого поле заводится:
// collectBySource обрывается на LIMIT и хвост попросту не видит, поэтому
// счётчик, снятый по дороге, назвал бы остаток нулём при непустой памяти.
// Ошибка была бы В НАШУ ПОЛЬЗУ и снаружи незаметна. Поэтому ниже есть
// TestVMEMQuarantineRemainderSurvivesLimitCutoff — он краснеет именно на этой
// подмене и ни на чём другом.
// =============================================================================

// remainderStore — расстановка, где КАЖДОЕ условие приговора отвергает свой
// факт: чужой scope, чужой источник, истёкший по TTL и лежащий раньше окна.
// Времена: записи 1000 и 3000, TTL истекает в 1100, карантин в 2000.
func remainderStore(t *testing.T, flushAt int) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	facts := []struct {
		id, source, scope string
		at, ttl           int64
	}{
		{"bad_old", "web-scraper", "user:a", 1000, 0},    // раньше окна → остаётся
		{"bad_seen", "web-scraper", "user:a", 3000, 0},   // замеченная ложь → снимается
		{"bad_gone", "web-scraper", "user:a", 1000, 100}, // истёк по TTL → не истина
		{"foreign", "web-scraper", "user:b", 1000, 0},    // чужая память
		{"human1", "human", "user:a", 1000, 0},           // соседний источник
	}
	for i, f := range facts {
		if _, err := lvs.Remember(RememberRequest{
			ID: f.id, Scope: f.scope, Text: "дедлайн проекта " + f.id,
			Source: f.source, ValidFrom: f.at, TTL: f.ttl,
		}, f.at); err != nil {
			t.Fatalf("Remember %s: %v", f.id, err)
		}
		if flushAt > 0 && i+1 == flushAt {
			lvs.FlushDeltaSync()
		}
	}
	return lvs
}

func wantRemainder(t *testing.T, got QuarantineRemainder, still, outside, over int) {
	t.Helper()
	want := QuarantineRemainder{StillTrusted: still, OutsideWindow: outside, OverLimit: over}
	if got != want {
		t.Errorf("остаток %+v, ожидался %+v", got, want)
	}
}

// TestVMEMQuarantineRemainderMirrorsVerdict — остаток есть ТОЧНОЕ зеркало
// приговора, а не «сколько всего лежит по этому имени». Считать иначе значило
// бы мерить размер памяти вместо полноты лечения: истёкший по TTL факт истиной
// уже не считается, чужой scope не наш, соседний источник не при чём.
// Прогон по всем размещениям LSM не формальность — проход остатка ходит по
// сегментам, а это другой код, чем проход по дельте.
func TestVMEMQuarantineRemainderMirrorsVerdict(t *testing.T) {
	for _, st := range vmemLSMStates {
		t.Run(st.name, func(t *testing.T) {
			lvs := remainderStore(t, st.flushAt(5))
			defer lvs.Close()

			req := QuarantineRequest{Scope: "user:a", Source: "web-scraper", Since: 3000}
			res, err := lvs.Quarantine(req, 2000)
			if err != nil {
				t.Fatalf("Quarantine: %v", err)
			}
			if len(res.Docs) != 1 {
				t.Fatalf("отозвано %d, ожидался ровно bad_seen", len(res.Docs))
			}
			// bad_old — единственный, кто остался истиной: bad_gone истёк,
			// bad_seen снят, foreign и human1 под предикат не подпадают.
			wantRemainder(t, res.Remainder, 1, 1, 0)

			// ⭐Вторая точка компромисса, измеренная тем же прибором: без окна
			// остаток уходит в ноль. Обе теперь предъявляются числом, и выбор
			// «дешевле или полнее» перестал быть молчаливым.
			res2, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000)
			if err != nil {
				t.Fatalf("Quarantine без окна: %v", err)
			}
			if len(res2.Docs) != 1 {
				t.Fatalf("без окна отозвано %d, ожидался bad_old", len(res2.Docs))
			}
			wantRemainder(t, res2.Remainder, 0, 0, 0)
		})
	}
}

// TestVMEMQuarantineRemainderSurvivesLimitCutoff — 🚨отрицательный контроль на
// подмену измерения. Скан кандидатов обрывается, как только партия набрана, и
// остаток, досчитанный по этому черновику, был бы нулём при пятнадцати живых
// фактах. Тест ставит LIMIT заведомо меньше корпуса, и обмануть его согласием
// счётчиков нельзя: over_limit сверяется с тем, что реально осталось в памяти.
func TestVMEMQuarantineRemainderSurvivesLimitCutoff(t *testing.T) {
	const (
		total = 20
		limit = 5
	)
	lvs := quarantineLimitStore(t, total, "bad", "user:a", 1000)
	defer lvs.Close()

	req := QuarantineRequest{Scope: "user:a", Source: "bad", Limit: limit}
	for call := 1; call*limit <= total; call++ {
		res, err := lvs.Quarantine(req, 2000)
		if err != nil {
			t.Fatalf("Quarantine (вызов %d): %v", call, err)
		}
		if len(res.Docs) != limit {
			t.Fatalf("вызов %d снял %d, ожидалось %d", call, len(res.Docs), limit)
		}
		// Остаток обязан таять ровно на снятое. Ноль на первом же вызове
		// означал бы счёт по черновику — ради этой строки тест и написан.
		left := total - call*limit
		wantRemainder(t, res.Remainder, left, 0, left)
		// Сверка с памятью, а не с арифметикой квитанций: они могли бы
		// сходиться между собой и расходиться с тем, что лежит в сторе.
		if got := total - quarantinedCount(lvs, total); got != left {
			t.Fatalf("вызов %d: в памяти не отозвано %d, квитанция обещает %d",
				call, got, left)
		}
	}
}

// TestVMEMQuarantineRemainderCountsUnrevocableLegacy — факт БЕЗ valid_from
// (записан до появления прикладного времени, лежит мимо Remember) не
// отзывается ни при каком окне: предикат приговора требует valid_from >= Since,
// и отсутствие атрибута его не проходит.
//
// ⭐Такой факт обязан быть НАЗВАН, а не пропасть из счёта. Ответ «отозвано 0»
// без остатка читается как «этот источник чист», хотя чистым он не стал —
// его просто нечем выбрать, ровно как absent в VMEM.COVERAGE.
func TestVMEMQuarantineRemainderCountsUnrevocableLegacy(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// Обычный факт того же источника: он отзовётся, и на фоне его успеха
	// неотзываемый сосед обязан остаться названным, а не потеряться.
	// (Заодно задаёт размерность стора — легаси кладётся мимо Remember.)
	if _, err := lvs.Remember(RememberRequest{
		ID: "modern", Scope: "user:a", Text: "дедлайн проекта март",
		Source: "web-scraper", ValidFrom: 1000,
	}, 1000); err != nil {
		t.Fatalf("Remember modern: %v", err)
	}

	legacy := RememberedDoc{
		ID:  "legacy",
		Vec: vmemPlaceholderVector("legacy", lvs.dim),
		Attrs: Attributes{
			Cat: map[string]string{vmemAttrScope: "user:a", vmemAttrSource: "web-scraper"},
			Num: map[string]float64{
				vmemAttrValidTo:   float64(vmemOpenValidTo),
				vmemAttrExpiresAt: float64(vmemOpenValidTo),
				vmemAttrImp:       0.5,
			},
		},
		Terms: []TermTF{{Term: "дедлайн", TF: 1}},
	}
	if err := lvs.AddDocTerms(legacy.ID, legacy.Vec, legacy.Attrs, legacy.Terms); err != nil {
		t.Fatalf("AddDocTerms legacy: %v", err)
	}

	res, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if len(res.Docs) != 1 {
		t.Fatalf("отозвано %d, ожидался ровно modern — legacy отзывать нечем", len(res.Docs))
	}
	wantRemainder(t, res.Remainder, 1, 1, 0)
}

// TestVMEMQuarantineRemainderIgnoresAlreadyRevoked — повторный отзыв не должен
// вернуть остаток обратно: уже отозванный факт истиной не считается, иначе
// идемпотентный вызов вечно докладывал бы о непролеченной памяти.
func TestVMEMQuarantineRemainderIgnoresAlreadyRevoked(t *testing.T) {
	lvs := quarantineLimitStore(t, 3, "bad", "user:a", 1000)
	defer lvs.Close()

	req := QuarantineRequest{Scope: "user:a", Source: "bad"}
	if _, err := lvs.Quarantine(req, 2000); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	res, err := lvs.Quarantine(req, 2500)
	if err != nil {
		t.Fatalf("повторный Quarantine: %v", err)
	}
	if len(res.Docs) != 0 {
		t.Fatalf("повторный отзыв снял %d — идемпотентность сломана", len(res.Docs))
	}
	wantRemainder(t, res.Remainder, 0, 0, 0)
	// Контроль диагноза: ноль получен не оттого, что стор пуст.
	if got := quarantinedCount(lvs, 3); got != 3 {
		t.Fatalf("ось отзыва стоит у %d фактов из 3 — сценарий не воспроизвёлся", got)
	}
}
