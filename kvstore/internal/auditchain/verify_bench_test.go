// ===========================================================================
// ЦЕНА ПРОВЕРКИ ЦЕПИ — ЗАМЕР ДО КОДА КОМАНДЫ АУДИТА.
//
// Решается один вопрос: можно ли назвать сверку цепи КОМАНДОЙ, или ей нужна
// контрольная точка. Проверка линейна по числу звеньев, а цепь не компактится
// и растёт вечно — значит, «работает на тестовом каталоге» ничего не говорит о
// том, что будет через год.
//
// ПОРОГ, НАЗНАЧЕННЫЙ ЗАРАНЕЕ: полная проверка ГОДОВОЙ цепи — не дороже 10 с.
// Откуда 10: это команда, которую оператор запускает руками и ждёт ответа
// глядя в терминал; всё, что дольше, обязано быть фоновой задачей с прогрессом,
// а не синхронным ответом на запрос. Годовая цепь при тике 1 с — 31 536 000
// звеньев по 113 Б = 3.56 ГБ.
//
// ⚠ЧТО ЭТОТ ЗАМЕР НЕ МЕРЯЕТ: холодный диск. Файл читается страничным кэшем
// после записи, поэтому число ниже — это ПОТОЛОК скорости, нижняя граница
// цены. На холодном чтении 3.56 ГБ добавится время носителя (на NVMe ~1-2 с,
// на сетевом диске может быть в разы больше). Экстраполяция названа
// экстраполяцией и в доки как измеренная не пойдёт.
//
// ---------------------------------------------------------------------------
// ЧИСЛА (i7-9750H, ext4, write_cache=write back, сеть; 3 прогона)
//
//	Verify в памяти, 1 000 звеньев      453 / 422 нс на звено
//	Verify в памяти, 100 000 звеньев    429 / 415 нс на звено   ← масштабируется линейно
//	ReadChain с диска, 100 000 звеньев  813 / 442 нс на звено
//
// В пересчёте на годовую цепь: Verify 13–14 с, чтение 14–26 с, ВМЕСТЕ 27–40 с.
//
// ⭐ВЕРДИКТ: ПОРОГ НЕ ВЗЯТ, И ЭТО МЕНЯЕТ КОМАНДУ, А НЕ ПОРОГ. Полная сверка с
// нуля не может быть синхронным ответом на запрос: через год она стоит
// полминуты, через три — полторы, и дальше только хуже, потому что цепь не
// компактится по построению. Поэтому:
//
//  1. `VMEM.AUDIT VERIFY` по умолчанию проверяет ОКНО, а не всё. Размер окна
//     взят из этих чисел: 10 с ÷ (415 нс + 442 нс) ≈ 11.6 млн звеньев,
//     округлено вниз до 10 000 000 (при тике 1 с это ~116 суток).
//  2. Полный проход остаётся доступным явно (`FROM 0`) и честно назван в
//     доках как операция на десятки секунд, растущая со временем.
//  3. ⭐Отдельная контрольная точка НЕ ВВОДИТСЯ, и это следствие П8, а не
//     экономия. Подписанное заявление (`VMEM.AUDIT EXPORT`) уже содержит
//     голову на свой момент, а аудитор его хранит. Значит контрольная точка
//     у него УЖЕ ЕСТЬ, и следующая сверка идёт от неё: `FROM <seq из
//     прошлого заявления>`. Своя же контрольная точка рядом с журналом не
//     добавила бы доверия — владелец, способный переписать цепь, перепишет и
//     её; она сэкономила бы только время, а время экономит окно.
//
// ===========================================================================
package auditchain

import (
	"fmt"
	"testing"
	"time"
)

// linksPerYear — звеньев в году при периоде агрегации 1 с (решение 5).
const linksPerYear = 31_536_000

// verifyBudget — порог: полная проверка годовой цепи.
const verifyBudget = 10 * time.Second

// BenchmarkVerifyChain — чистая проверка связности и хешей в памяти.
func BenchmarkVerifyChain(b *testing.B) {
	for _, n := range []int{1_000, 100_000} {
		b.Run(fmt.Sprintf("links=%d", n), func(b *testing.B) {
			links, head := buildLinkChain(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Verify(links, &head); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			perLink := time.Duration(b.Elapsed().Nanoseconds() / int64(b.N) / int64(n))
			year := perLink * linksPerYear
			b.Logf("%v на звено → годовая цепь (%d звеньев) проверяется за %v, порог %v [ЭКСТРАПОЛЯЦИЯ]",
				perLink, linksPerYear, year.Round(time.Second), verifyBudget)
		})
	}
}

// BenchmarkReadChain — разбор цепи с диска: кадры, CRC, декодирование.
// Именно это, а не Verify, обычно и оказывается дороже.
func BenchmarkReadChain(b *testing.B) {
	const n = 100_000
	dir := benchDir(b)
	reportEnv(b, dir)

	c, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		c.Append(leafForBench(i))
		if _, err := c.Flush(); err != nil { // одно звено на событие — цепь нужной длины
			b.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		links, err := ReadChain(dir)
		if err != nil {
			b.Fatal(err)
		}
		if len(links) != n {
			b.Fatalf("прочитано %d звеньев из %d", len(links), n)
		}
	}
	b.StopTimer()
	perLink := time.Duration(b.Elapsed().Nanoseconds() / int64(b.N) / int64(n))
	year := perLink * linksPerYear
	b.Logf("%v на звено → чтение годовой цепи %v, порог %v [ЭКСТРАПОЛЯЦИЯ, страничный кэш горячий]",
		perLink, year.Round(time.Second), verifyBudget)
}

func leafForBench(i int) Leaf {
	return Leaf{
		UnixNano: int64(1_753_700_000_000_000_000 + i),
		Type:     EventRemember,
		Scope:    "user:nikolay",
		Subject:  fmt.Sprintf("01KYSEE4S0JC03XEEVR4H40%03d", i%1000),
		Payload:  []byte(`{"h":"3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","src":"agent-a","sealed":true}`),
	}
}

// buildLinkChain — цепь из n звеньев в памяти.
func buildLinkChain(b *testing.B, n int) ([]Record, Head) {
	b.Helper()
	var head Head
	links := make([]Record, 0, n)
	batch := []Leaf{leafForBench(0)}
	for i := 0; i < n; i++ {
		r, err := LinkBatch(head, int64(i), uint64(i), batch)
		if err != nil {
			b.Fatal(err)
		}
		links = append(links, r)
		head = Head{Seq: r.Seq, Hash: Hash(r)}
	}
	return links, head
}
