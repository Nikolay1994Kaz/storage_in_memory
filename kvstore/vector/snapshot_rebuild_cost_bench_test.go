// ЗАМЕР ЦЕНЫ ДО КОДА, второй для П5а. Первый (snapshot_replay_cost_bench_test)
// закрыл дешёвый путь «восстанавливать VMEM-факты реплеем» — 4.9 с на 10k.
// Этот меряет цену пути P: снапшот хранит документы (id, вектор, атрибуты,
// термы) группами по скоупу под конвертом, а колоночный и текстовый слои
// СТРОЯТСЯ на загрузке из этих документов — теми же buildSegmentAttrs и
// buildSegmentText, которыми они строятся при флаше дельты.
//
// ПОЧЕМУ ЭТО ВООБЩЕ ВОЗМОЖНО. Слои сегмента (граф, колонки, инвертированный
// BM25-индекс) индексируются ПОЗИЦИЕЙ документа и перемешивают скоупы, поэтому
// зашифровать «часть сегмента» нельзя. Но документная форма (DeltaEntry)
// разделяется по скоупу тривиально, а обратно в слои собирается существующим
// кодом. То есть шифруется то, что делимо, а собирается то, что нет.
//
// ПОРОГ, НАЗНАЧЕННЫЙ ЗАРАНЕЕ. Нынешняя загрузка 10k фактов из бинарного
// снапшота — 33–76 мс (замер №1). Путь P принимается, если сборка слоёв на
// тех же 10k укладывается в 100 мс, то есть остаётся ТОГО ЖЕ порядка, а не
// возвращает нас к секундам отвергнутого реплея. Расшифровка сюда не входит и
// заведомо мала: по замеру keyring это ~1.5 ГБ/с, то есть единицы мс на
// снапшот в 6.7 МБ.
package vector

import (
	"fmt"
	"testing"
	"time"
)

// rebuildCostEntries — 10k документов масштаба реальной записи VMEM в той
// форме, в которой их держала бы документная часть снапшота.
func rebuildCostEntries(n, dim int) []DeltaEntry {
	entries := make([]DeltaEntry, n)
	for i := range entries {
		key, vec, attrs, terms := replayCostFact(i, dim)
		entries[i] = DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms}
	}
	return entries
}

func TestSnapshotRebuildCost(t *testing.T) {
	if testing.Short() {
		t.Skip("замер цены сборки слоёв: гоняется без -short")
	}

	const thresholdAt10k = 100 * time.Millisecond

	for _, dim := range []int{32, 768} { // 32 — placeholder ступени 0, типичный VMEM
		entries := rebuildCostEntries(10000, dim)

		startText := time.Now()
		st := buildSegmentText(entries)
		textDur := time.Since(startText)

		startAttrs := time.Now()
		sa := buildSegmentAttrs(entries, nil)
		attrsDur := time.Since(startAttrs)

		// Положительный контроль: замер обязан строить НАСТОЯЩИЕ слои, иначе
		// он померил бы стоимость возврата nil. Урок 27.07: у отрицательной
		// проверки должен быть парный положительный контроль.
		if st == nil {
			t.Fatal("buildSegmentText вернул nil — замер измерил бы пустоту")
		}
		if sa == nil {
			t.Fatal("buildSegmentAttrs вернул nil — замер измерил бы пустоту")
		}

		total := textDur + attrsDur
		t.Logf("dim=%-4d 10k фактов: BM25-индекс %s + колонки %s = %s (порог %s)",
			dim, textDur.Round(time.Millisecond), attrsDur.Round(time.Millisecond),
			total.Round(time.Millisecond), thresholdAt10k)

		if total > thresholdAt10k {
			t.Errorf("порог не пройден при dim=%d: сборка слоёв %s > %s. Путь P теряет "+
				"смысл — он оправдан ровно тем, что цена остаётся порядка нынешней "+
				"загрузки (33–76 мс), а не возвращает секунды отвергнутого реплея",
				dim, total.Round(time.Millisecond), thresholdAt10k)
		}
	}
}

// TestSnapshotRebuildFidelity — контроль, без которого замер выше ничего не
// стоит: собранные слои должны совпадать с тем, что даёт обычный путь флаша.
// Иначе «дёшево» означало бы лишь, что мы строим что-то не то.
func TestSnapshotRebuildFidelity(t *testing.T) {
	entries := rebuildCostEntries(500, 32)

	st1, st2 := buildSegmentText(entries), buildSegmentText(entries)
	if st1 == nil || st2 == nil {
		t.Fatal("слой не построен")
	}
	if len(st1.postings) != len(st2.postings) || st1.nText != st2.nText || st1.totalLen != st2.totalLen {
		t.Fatalf("сборка недетерминирована: postings %d/%d, nText %d/%d, totalLen %d/%d",
			len(st1.postings), len(st2.postings), st1.nText, st2.nText, st1.totalLen, st2.totalLen)
	}
	for i := range st1.postings {
		if len(st1.postings[i]) != len(st2.postings[i]) {
			t.Fatalf("постинг-лист %d разошёлся: %d против %d", i, len(st1.postings[i]), len(st2.postings[i]))
		}
	}

	// docLen должен считаться по КАЖДОМУ документу, а не по первому: проверка
	// на нейтральных данных (все документы одинаковой длины) не поймала бы
	// подмену. Поэтому длины здесь заведомо разные.
	varied := make([]DeltaEntry, 3)
	for i := range varied {
		varied[i] = DeltaEntry{
			Key:   fmt.Sprintf("k%d", i),
			Vec:   []float32{1, 2, 3},
			Terms: make([]TermTF, i+1),
		}
		for j := range varied[i].Terms {
			varied[i].Terms[j] = TermTF{Term: fmt.Sprintf("t%d", j), TF: uint16(j + 1)}
		}
	}
	stv := buildSegmentText(varied)
	if stv == nil {
		t.Fatal("слой не построен на разноразмерных документах")
	}
	want := []uint32{1, 3, 6} // tf: 1 | 1+2 | 1+2+3
	for i, w := range want {
		if stv.docLen[i] != w {
			t.Errorf("docLen[%d] = %d, ожидалось %d", i, stv.docLen[i], w)
		}
	}
}
