package vector

import (
	"strings"
	"testing"

	"kvstore/kvstore/internal/vmemcorpus"
)

// Деградация качества recall при накоплении ГРЯЗИ.
//
// ЗАЧЕМ. Канон шага 8 (`TestVMEMCorpusBench`) судит ЧИСТЫЙ корпус: каждый факт
// там либо нужен, либо перекрыт supersession, либо явно забыт. Реальная память
// пачкается иначе, и обзоры называют это главным практическим отказом — память
// портится сама, без атакующего, и через месяц работы «ничто не решает, чего не
// помнить». Ни один наш замер этого не трогал: ни канон (чистый корпус), ни
// LongMemEval (48 готовых документов на вопрос, никакого накопления).
//
// ЧТО ИМЕННО МЕРИТСЯ. Те же вопросы, тот же движок, разная доля грязи. Вопросы
// не меняются по построению (см. TestCorpusNoiseDoesNotDisturbCleanTape),
// поэтому падение метрики списывается на корпус, а не на смену вопросов.
//
// ⭐КОНТРОЛЬ — строка share=0 В ЭТОМ ЖЕ ПРОГОНЕ, а не число канона: корпус здесь
// уменьшен ради времени, и сравнивать его с каноном значило бы сравнивать два
// разных мира. Канон отвечает «сколько», этот тест — «насколько хуже».
//
// ПОРОГИ ОБЪЯВЛЕНЫ ДО ПРОГОНА (норма проекта):
//   - падение known hit@1 при 80% грязи БОЛЬШЕ 15 пунктов → движок тонет в
//     шуме, это дефект и повод чинить;
//   - падение МЕНЬШЕ 1 пункта → подозрительно: скорее всего грязь не доехала
//     или не конкурирует, и тест обязан это доказать отдельно (см. контроль
//     «шум виден в выдаче»), а не поверить в собственную устойчивость.
//
// 🚨ЧЕСТНАЯ ПОМЕТКА О ПОРОГЕ PARA. Первый прогон показал, что я сторожил не ту
// колонку: known упал на 5.2 пункта (порог выдержан), а para — на 15.9, то есть
// втрое сильнее. Para и есть реалистичная форма запроса: опознавательный токен
// человек обычно не помнит, он описывает, чего хочет. Порог paraAlarm объявлен
// ПОСЛЕ первого прогона и потому предсказанием НЕ является — он сторожит
// ухудшение впредь, а не подтверждает гипотезу.
//
// ⚠ЧЕГО ЭТИ ЧИСЛА НЕ ЗНАЧАТ. Правда здесь — на уровне ID: попадание засчитано,
// только если вернулся ТОТ САМЫЙ факт. Формы dup и restate по построению несут
// почти то же содержание, что источник, поэтому их выход наверх считается
// промахом, хотя пользователь получил бы факт с тем же смыслом. Часть падения
// para — артефакт правды-по-id, а не потеря информации. Отличить «не тот факт»
// от «тот же факт другой строкой» умела бы только правда по содержанию, а её у
// нас нет.
const (
	noiseDropAlarm   = 0.15 // known: выше — тонем (объявлен ДО прогона)
	noiseDropSuspect = 0.01 // ниже — проверить, что грязь вообще участвует
	noiseParaAlarm   = 0.22 // para: объявлен ПОСЛЕ первого прогона (0.159), сторож впредь
)

func noiseSweepParams(share float64) vmemcorpus.Params {
	p := vmemcorpus.Default()
	// Уменьшенный корпус: замер про ОТНОШЕНИЕ, а не про канонные абсолюты.
	p.PlainFacts, p.TTLFacts, p.ForgetFacts = 6000, 800, 150
	p.Chains, p.ProbeGroups, p.Vocab = 200, 20, 8000
	p.KnownQ, p.ParaQ = 1200, 1200
	p.AsOfQ, p.NowChainQ, p.ErasureQ = 100, 50, 50
	p.NoiseShare = share
	return p
}

type noiseResult struct {
	share               float64
	facts               int
	knownHit1, knownMRR float64
	paraHit1, paraMRR   float64
	noiseInTop10        int // сколько раз грязь попала в выдачу known-запросов
}

func TestVMEMNoiseDegradation(t *testing.T) {
	if testing.Short() {
		t.Skip("замер деградации: только полный прогон")
	}
	shares := []float64{0, 0.3, 0.6, 0.8}
	for _, hybrid := range []bool{false, true} {
		mode := "stage0-bm25"
		if hybrid {
			mode = "hybrid-bow"
		}
		t.Run(mode, func(t *testing.T) {
			var res []noiseResult
			for _, s := range shares {
				res = append(res, runNoiseArm(t, s, hybrid))
			}
			base := res[0]
			t.Logf("%-6s %-8s %-10s %-10s %-10s %-10s %s", "грязь", "фактов",
				"known@1", "knownMRR", "para@1", "paraMRR", "грязи в топ-10")
			for _, r := range res {
				t.Logf("%-6.0f%% %-8d %-10.3f %-10.3f %-10.3f %-10.3f %d",
					r.share*100, r.facts, r.knownHit1, r.knownMRR,
					r.paraHit1, r.paraMRR, r.noiseInTop10)
			}
			last := res[len(res)-1]
			drop := base.knownHit1 - last.knownHit1
			t.Logf("падение known hit@1 при %.0f%% грязи: %.3f (порог тревоги %.2f)",
				last.share*100, drop, noiseDropAlarm)

			// контроль «грязь доехала и участвует»: без него нулевое падение
			// читалось бы как устойчивость движка, хотя означало бы, что мы
			// ничего не подмешали.
			if last.facts <= base.facts {
				t.Fatalf("корпус не вырос: %d → %d, грязь не доехала", base.facts, last.facts)
			}
			if last.noiseInTop10 == 0 {
				t.Fatalf("грязь ни разу не попала в топ-10: она не конкурирует, "+
					"и %.3f падения ничего не доказывают", drop)
			}
			if drop > noiseDropAlarm {
				t.Errorf("known hit@1 упал на %.3f при %.0f%% грязи — движок тонет в шуме",
					drop, last.share*100)
			}
			// ⭐Para падает ВТРОЕ сильнее known, и para — реалистичная форма
			// запроса. Порог post-hoc (см. шапку), сторожит ухудшение впредь.
			paraDrop := base.paraHit1 - last.paraHit1
			t.Logf("падение para hit@1: %.3f — ВТРОЕ больше known; опознавательный "+
				"токен спасает, описание нет", paraDrop)
			if paraDrop > noiseParaAlarm {
				t.Errorf("para hit@1 упал на %.3f при %.0f%% грязи (порог %.2f)",
					paraDrop, last.share*100, noiseParaAlarm)
			}
			if drop < noiseDropSuspect {
				t.Logf("⚠падение %.3f ниже порога подозрения %.2f — при живом контроле "+
					"выше это значит, что грязь конкурирует, но не вытесняет", drop, noiseDropSuspect)
			}
		})
	}
}

func runNoiseArm(t *testing.T, share float64, hybrid bool) noiseResult {
	t.Helper()
	c := vmemcorpus.Generate(noiseSweepParams(share))

	cfg := bm25TestConfig()
	cfg.DeltaMax = 4096
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()

	const day = int64(86400)
	nextReap := c.Events[0].At + 15*day
	facts := 0
	for _, ev := range c.Events {
		for ev.At >= nextReap {
			lvs.ReapExpired(nextReap, 1<<20)
			nextReap += 15 * day
		}
		if ev.Fact == nil {
			if _, err := lvs.ForgetInScope(ev.ForgetID, ev.ForgetScope); err != nil {
				t.Fatalf("Forget %s: %v", ev.ForgetID, err)
			}
			continue
		}
		f := ev.Fact
		req := RememberRequest{
			ID: f.ID, Scope: f.Scope, Text: f.Text, Type: f.Type,
			TTL: f.TTLSec, Supersedes: f.Supersedes,
		}
		if f.Imp >= 0 {
			imp := f.Imp
			req.Importance = &imp
		}
		if hybrid {
			req.Vector = f.Vec
		}
		if _, err := lvs.Remember(req, ev.At); err != nil {
			t.Fatalf("Remember %s: %v", f.ID, err)
		}
		facts++
	}
	lvs.ReapExpired(c.NowV, 1<<20)
	lvs.FlushDeltaSync()

	r := noiseResult{share: share, facts: facts}
	var knownN, paraN int
	for _, q := range c.Queries {
		if q.Sort != "known" && q.Sort != "para" {
			continue
		}
		req := RecallRequest{Scope: q.Scope, Query: q.Text, K: q.K}
		if hybrid {
			req.Vector = q.Vec
		}
		out, err := lvs.Recall(req, c.NowV)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		rank := 0
		for i, d := range out {
			if strings.HasPrefix(d.Key, "noise:") && q.Sort == "known" {
				r.noiseInTop10++
			}
			if d.Key == q.WantID && rank == 0 {
				rank = i + 1
			}
		}
		hit1, mrr := 0.0, 0.0
		if rank == 1 {
			hit1 = 1
		}
		if rank > 0 {
			mrr = 1 / float64(rank)
		}
		if q.Sort == "known" {
			knownN++
			r.knownHit1 += hit1
			r.knownMRR += mrr
		} else {
			paraN++
			r.paraHit1 += hit1
			r.paraMRR += mrr
		}
	}
	if knownN > 0 {
		r.knownHit1 /= float64(knownN)
		r.knownMRR /= float64(knownN)
	}
	if paraN > 0 {
		r.paraHit1 /= float64(paraN)
		r.paraMRR /= float64(paraN)
	}
	return r
}
