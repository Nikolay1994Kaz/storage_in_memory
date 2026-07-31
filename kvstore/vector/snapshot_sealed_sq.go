package vector

import (
	"fmt"
	"io"
	"unsafe"
)

// SQ8-сегмент под конвертом: то же, что snapshot_sealed_segment.go делает для
// float32, но для квантованного формата.
//
// ПОЧЕМУ ОТДЕЛЬНЫМ ФАЙЛОМ, А НЕ ВЕТКОЙ В ТОМ ЖЕ. Делители набора документов
// (splitBySealed, sealedOnly, mergeSealedIntoEntries, vmemScopeOf) работают на
// []DeltaEntry и типа сегмента не знают — они переиспользуются как есть. Типом
// связаны ровно две операции: развернуть сегмент в документы и вписать векторы
// обратно. У SQ8 обе проходят через квантование, и держать их рядом с
// float32-версиями значило бы прятать существенную разницу за похожими именами.
//
// ⭐ОБРАТИМОСТЬ. frozenSQEntries деквантует коды в float32, документная секция
// хранит эти float32, restoreSealedVectorsSQ квантует их обратно ТОЙ ЖЕ
// калибровкой сегмента. Квантование детерминировано, калибровка не меняется —
// значит round-trip восстанавливает коды бит в бит. Потери относительно
// исходного вектора уже произошли при заморозке; шифрование их не добавляет.

// writeCodesMasked пишет слэб кодов, подменяя НУЛЯМИ позиции из mask.
// Зеркало writeSlabMasked, отличается шагом: у SQ8 позиция занимает dim байт,
// а не dim*4. Пишет непрерывными кусками — копия всего слэба стоила бы столько
// же памяти, сколько сам индекс, а замаскированных обычно меньшинство.
func writeCodesMasked(w io.Writer, codes []uint8, dim, n int, mask []bool) error {
	zeros := make([]byte, dim)
	runStart := 0

	flush := func(end int) error {
		if end <= runStart {
			return nil
		}
		b := unsafe.Slice((*byte)(unsafe.Pointer(&codes[runStart*dim])), (end-runStart)*dim)
		_, err := w.Write(b)
		return err
	}

	for i := 0; i < n; i++ {
		if i >= len(mask) || !mask[i] {
			continue
		}
		if err := flush(i); err != nil {
			return fmt.Errorf("frozenSQ: write codes: %w", err)
		}
		if _, err := w.Write(zeros); err != nil {
			return fmt.Errorf("frozenSQ: write masked codes: %w", err)
		}
		runStart = i + 1
	}
	if err := flush(n); err != nil {
		return fmt.Errorf("frozenSQ: write codes tail: %w", err)
	}
	return nil
}

// frozenSQEntries разворачивает SQ8-сегмент в документы — зеркало
// frozenEntries. Отличие одно и существенное: вектор не берётся видом в слэб, а
// ДЕКВАНТУЕТСЯ в новый слайс (в кодах его иначе не достать). Позиция
// сохраняется: дыры остаются дырами, все слои адресуются позицией.
func frozenSQEntries(s *frozenSQSegment) []DeltaEntry {
	fg := s.fg
	decTerms := s.text.decodeTerms()
	entries := make([]DeltaEntry, fg.n)
	for i := 0; i < fg.n; i++ {
		key := fg.keys.view(i)
		if key == "" {
			continue // дыра: нода удалена
		}
		var terms []TermTF
		if i < len(decTerms) {
			terms = decTerms[i]
		}
		entries[i] = DeltaEntry{
			Key:   fg.keys.clone(i),
			Vec:   fg.dequantAt(i),
			Attrs: s.attrs.decodeAt(i),
			Terms: terms,
		}
	}
	return entries
}

// dequantAt — вектор позиции i в float32. Формула та же, что в горячем пути
// обхода (sqBruteDist): v[d] = sqMin[d] + code[d]*sqScale[d].
func (fg *FrozenGraphSQ) dequantAt(i int) []float32 {
	if fg.dim == 0 || i >= fg.n {
		return nil
	}
	out := make([]float32, fg.dim)
	base := i * fg.dim
	for d := 0; d < fg.dim; d++ {
		out[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
	}
	return out
}

// restoreSealedVectorsSQ вписывает векторы восстановленных документов обратно в
// коды: на диске они были занулены. Позиции стёртых остаются нулевыми.
//
// Квантование повторяет FreezeGraphSQ дословно — включая обработку
// константной размерности (sqScale==0 → код 0) и округление к ближайшему.
// Разойдись эти две формулы, факт возвращался бы из конверта СДВИНУТЫМ, и
// поиск по нему тихо деградировал бы только для запечатанных скоупов.
func restoreSealedVectorsSQ(fg *FrozenGraphSQ, entries []DeltaEntry) {
	for i, e := range entries {
		if e.Key == "" || len(e.Vec) != fg.dim || i >= fg.n {
			continue
		}
		base := i * fg.dim
		for d := 0; d < fg.dim; d++ {
			if fg.sqScale[d] == 0 {
				fg.codes[base+d] = 0
				continue
			}
			q := (e.Vec[d] - fg.sqMin[d]) / fg.sqScale[d]
			qi := int(q + 0.5)
			if qi < 0 {
				qi = 0
			} else if qi > 255 {
				qi = 255
			}
			fg.codes[base+d] = uint8(qi)
		}
	}
}
