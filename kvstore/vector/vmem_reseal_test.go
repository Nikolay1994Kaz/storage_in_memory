package vector

import "testing"

// ⭐TestResealKeys_RechecksUnderLock — вторая фаза перешифровки перепроверяет
// предикат под эксклюзивным замком.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Скан и приговор разнесены во времени, и между ними
// факт может быть переписан заново — уже под конвертом. Сканом такой ключ уже
// отобран, и без перепроверки перешифровка записала бы ЛИШНЮЮ версию факта:
// объём журнала вырос бы, а смысл не изменился. Командные тесты этого не
// видят, потому что через команду скан и приговор идут подряд: там кандидат
// физически не успевает измениться. Мутация «снять перепроверку» уходила
// именно поэтому — поймано мутационным прогоном.
func TestResealKeys_RechecksUnderLock(t *testing.T) {
	const now = int64(1_753_000_000)
	lvs := NewLeveledVectorStore(LeveledConfig{Distance: EuclideanDistance, NumBuilders: 1})

	res, err := lvs.Remember(RememberRequest{
		Scope: "alice", Text: "уже под конвертом", Source: "agent-a", SealedAtRest: true,
	}, now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// ПАРНЫЙ КОНТРОЛЬ: скан такой факт и не отберёт — значит через команду до
	// второй фазы он не дошёл бы, и проверка ниже осмысленна только напрямую.
	if cands := lvs.collectUnsealed("alice", 10); len(cands) != 0 {
		t.Fatalf("скан отобрал уже покрытый факт: %v", cands)
	}

	// Приговор зовём НАПРЯМУЮ с этим ключом — модель того, что кандидат успел
	// стать покрытым между фазами.
	out, err := lvs.resealKeys([]string{res.Doc.ID}, ResealRequest{Scope: "alice"}, now)
	if err != nil {
		t.Fatalf("resealKeys: %v", err)
	}
	if len(out.Docs) != 0 {
		t.Fatalf("перешифрован уже покрытый факт: %d версий записано лишней работой", len(out.Docs))
	}
}

// ⭐TestCollectUnsealed_StaysInsideScope — граница скоупа держится уже на
// СКАНЕ, а не только на приговоре.
//
// ⚠Мутация «снять фильтр по скоупу в скане» уходила от командных тестов: до
// чужих фактов дело не доходило, их отсеивала вторая фаза, и наружу эффекта не
// было. Но эффект есть, просто другой — у скана есть ЛИМИТ, и чужие факты его
// съедают. Скоуп с двумя непокрытыми фактами рядом с тысячей чужих был бы
// перешифрован частично или не перешифрован вовсе, а квитанция показала бы
// честный ноль, который невозможно объяснить.
func TestCollectUnsealed_StaysInsideScope(t *testing.T) {
	const now = int64(1_753_000_000)
	lvs := NewLeveledVectorStore(LeveledConfig{Distance: EuclideanDistance, NumBuilders: 1})

	var aliceID string
	for _, tc := range []struct{ scope, text string }{
		{"alice", "факт алисы"},
		{"bob", "первый факт боба"},
		{"bob", "второй факт боба"},
		{"bob", "третий факт боба"},
	} {
		res, err := lvs.Remember(RememberRequest{Scope: tc.scope, Text: tc.text, Source: "agent-a"}, now)
		if err != nil {
			t.Fatalf("Remember(%s): %v", tc.scope, err)
		}
		if tc.scope == "alice" {
			aliceID = res.Doc.ID
		}
	}

	got := lvs.collectUnsealed("alice", 10)
	if len(got) != 1 || got[0] != aliceID {
		t.Fatalf("скан отобрал %v, ожидался ровно один ключ алисы (%s)", got, aliceID)
	}
	// И с лимитом в один: чужие факты не имеют права его занять.
	if got := lvs.collectUnsealed("alice", 1); len(got) != 1 || got[0] != aliceID {
		t.Fatalf("под лимитом 1 скан отобрал %v — бюджет съеден чужим скоупом", got)
	}
}

// TestResealKeys_SkipsForeignScope — вторая фаза проверяет и скоуп: между
// фазами факт мог уехать в другую память.
func TestResealKeys_SkipsForeignScope(t *testing.T) {
	const now = int64(1_753_000_000)
	lvs := NewLeveledVectorStore(LeveledConfig{Distance: EuclideanDistance, NumBuilders: 1})

	res, err := lvs.Remember(RememberRequest{Scope: "bob", Text: "факт боба", Source: "agent-a"}, now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	out, err := lvs.resealKeys([]string{res.Doc.ID}, ResealRequest{Scope: "alice"}, now)
	if err != nil {
		t.Fatalf("resealKeys: %v", err)
	}
	if len(out.Docs) != 0 {
		t.Fatal("перешифровка ушла в чужой скоуп")
	}
}

// TestResealKeys_SkipsExpired — истёкший факт перешифровывать нечего: он уже
// невидим на чтении, а физически его снимет жнец. Иначе результат зависел бы
// от расписания жнеца.
func TestResealKeys_SkipsExpired(t *testing.T) {
	const now = int64(1_753_000_000)
	lvs := NewLeveledVectorStore(LeveledConfig{Distance: EuclideanDistance, NumBuilders: 1})

	res, err := lvs.Remember(RememberRequest{Scope: "alice", Text: "недолгий", Source: "agent-a", TTL: 10}, now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// ПАРНЫЙ КОНТРОЛЬ: пока не истёк — перешифровывается.
	if out, err := lvs.resealKeys([]string{res.Doc.ID}, ResealRequest{Scope: "alice"}, now); err != nil || len(out.Docs) != 1 {
		t.Fatalf("живой факт не перешифрован: docs=%d err=%v", len(out.Docs), err)
	}
	if out, err := lvs.resealKeys([]string{res.Doc.ID}, ResealRequest{Scope: "alice"}, now+100); err != nil || len(out.Docs) != 0 {
		t.Fatalf("истёкший факт перешифрован: docs=%d err=%v", len(out.Docs), err)
	}
}
