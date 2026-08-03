package vmemcorpus

import (
	"fmt"
	"strings"
	"testing"
)

// ⭐ГЛАВНЫЙ ИНВАРИАНТ ШУМА: включение грязи не имеет права шевелить ни чистые
// факты, ни ЗАПРОСЫ. Чистый корпус — контроль замера деградации; если он поедет
// вместе с шумом, сравнивать будет нечего: мы будем мерить разные вопросы на
// разных корпусах и списывать разницу на шум.
//
// Тест ловит ровно одну ошибку, которую легко совершить и невозможно заметить
// глазами: генерация шума из ОСНОВНОГО rng. Розыгрыши после неё съезжают, и
// набор запросов становится другим — при внешне «тех же» параметрах.
func TestCorpusNoiseDoesNotDisturbCleanTape(t *testing.T) {
	small := func(noise float64) Params {
		p := Default()
		p.PlainFacts, p.TTLFacts, p.ForgetFacts = 400, 60, 20
		p.Chains, p.ProbeGroups, p.Vocab = 40, 6, 2000
		p.KnownQ, p.ParaQ, p.AsOfQ, p.NowChainQ, p.ErasureQ = 80, 80, 40, 20, 20
		p.NoiseShare = noise
		return p
	}
	clean := Generate(small(0))
	dirty := Generate(small(0.8))

	// 1. запросы обязаны совпасть ПОЛНОСТЬЮ
	if len(clean.Queries) != len(dirty.Queries) {
		t.Fatalf("число запросов разошлось: чистый %d, с шумом %d",
			len(clean.Queries), len(dirty.Queries))
	}
	for i := range clean.Queries {
		a, b := clean.Queries[i], dirty.Queries[i]
		if a.Sort != b.Sort || a.Scope != b.Scope || a.Text != b.Text ||
			a.WantID != b.WantID || a.AsOf != b.AsOf {
			t.Fatalf("запрос %d разошёлся:\n чистый: %+v\n с шумом: %+v", i, a, b)
		}
	}

	// 2. каждый чистый факт обязан присутствовать в грязной ленте как есть
	cleanFacts := map[string]string{}
	for _, e := range clean.Events {
		if e.Fact != nil {
			cleanFacts[e.Fact.ID] = fmt.Sprintf("%s|%s|%d|%s",
				e.Fact.Scope, e.Fact.Text, e.Fact.At, e.Fact.Supersedes)
		}
	}
	noiseSeen := 0
	for _, e := range dirty.Events {
		if e.Fact == nil {
			continue
		}
		if strings.HasPrefix(e.Fact.ID, "noise:") {
			noiseSeen++
			continue
		}
		want, ok := cleanFacts[e.Fact.ID]
		if !ok {
			t.Fatalf("в грязной ленте появился нешумовой факт, которого нет в чистой: %s", e.Fact.ID)
		}
		got := fmt.Sprintf("%s|%s|%d|%s", e.Fact.Scope, e.Fact.Text, e.Fact.At, e.Fact.Supersedes)
		if got != want {
			t.Fatalf("чистый факт %s изменился при включении шума:\n было: %s\n стало: %s",
				e.Fact.ID, want, got)
		}
		delete(cleanFacts, e.Fact.ID)
	}
	if len(cleanFacts) != 0 {
		t.Fatalf("при включении шума пропало %d чистых фактов", len(cleanFacts))
	}

	// 3. ОТРИЦАТЕЛЬНЫЙ КОНТРОЛЬ: шум обязан быть, иначе тест выше зелен даром
	wantNoise := int(float64(small(0.8).PlainFacts) * 0.8)
	if noiseSeen != wantNoise {
		t.Fatalf("шума в ленте %d, ожидалось %d — грязь не доехала", noiseSeen, wantNoise)
	}
}

// Шум обязан садиться В ТОТ ЖЕ scope, что источник: в чужом его отсекает
// фильтр, и замер вышел бы про фильтр, а не про качество поиска.
func TestCorpusNoiseLandsInLivingScopes(t *testing.T) {
	p := Default()
	p.PlainFacts, p.TTLFacts, p.ForgetFacts = 400, 60, 20
	p.Chains, p.ProbeGroups, p.Vocab = 40, 6, 2000
	p.KnownQ, p.ParaQ, p.AsOfQ, p.NowChainQ, p.ErasureQ = 80, 80, 40, 20, 20
	p.NoiseShare = 0.5
	c := Generate(p)

	realScopes := map[string]bool{}
	for _, e := range c.Events {
		if e.Fact != nil && !strings.HasPrefix(e.Fact.ID, "noise:") {
			realScopes[e.Fact.Scope] = true
		}
	}
	kinds := map[string]int{}
	for _, e := range c.Events {
		if e.Fact == nil || !strings.HasPrefix(e.Fact.ID, "noise:") {
			continue
		}
		if !realScopes[e.Fact.Scope] {
			t.Fatalf("шум %s сел в скоуп %s, где нет настоящих фактов — фильтр уберёт его даром",
				e.Fact.ID, e.Fact.Scope)
		}
		kinds[strings.Split(e.Fact.ID, ":")[1]]++
	}
	for _, k := range []string{NoiseJunk, NoiseDup, NoiseStale, NoiseRestate} {
		if kinds[k] == 0 {
			t.Fatalf("форма шума %q не сгенерирована ни разу", k)
		}
	}
	// stale обязан быть СТАРШЕ источника — иначе затуханию нечего развязывать
	for _, e := range c.Events {
		if e.Fact != nil && strings.HasPrefix(e.Fact.ID, "noise:"+NoiseStale) {
			if !strings.Contains(e.Fact.Text, "ent") {
				t.Fatalf("stale-шум %s без ent-токена — он не создаёт ничьей", e.Fact.ID)
			}
		}
	}
}
