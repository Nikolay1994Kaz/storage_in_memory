package vector

import (
	"fmt"
	"io"
)

// =============================================================================
// Durability текстового BM25-слоя (шаг 5 спринта, docs/BM25_HYBRID_DESIGN.md).
//
// В WAL и снапшоте едут ТЕРМЫ, не сырой текст: токенайзер вызывается ровно один
// раз в жизни дока (AddDoc), реплей/загрузка воспроизводят состояние бит-в-бит
// независимо от версии стеммера — журнал самодостаточен. Цена (записана в
// дизайне): смена стеммера = re-ingest корпуса.
//
// Форматы (little-endian, примитивы attr_durability.go):
//   terms:   [u32 nTerms] [term(str) tf(u16)]×n            — пер-доковый список
//            (WAL-op OpVSimAddDoc и hnsw-путь снапшота; инвариант
//            bm25CountTerms: термы уникальны и отсортированы — проверяется)
//   segText: [1B present]; если 1 →
//              [u32 nDocs][docLen(u32)×nDocs]
//              [u32 nTerms]; для termID 0..n-1:
//                [term(str)][u32 nPost][(doc u32)(tf u16)]×P
//            — колоночный слой frozen/frozenSQ-сегментов как есть; словарь
//            восстанавливается интернированием в порядке termID.
// =============================================================================

// writeTerms сериализует пер-доковый список термов (nil → nTerms=0).
func writeTerms(w io.Writer, terms []TermTF) error {
	if err := putU32(w, uint32(len(terms))); err != nil {
		return err
	}
	var u2 [2]byte
	for _, tt := range terms {
		if err := writeStr(w, tt.Term); err != nil {
			return err
		}
		u2[0] = byte(tt.TF)
		u2[1] = byte(tt.TF >> 8)
		if _, err := w.Write(u2[:]); err != nil {
			return err
		}
	}
	return nil
}

// readTerms — обратная операция. Валидирует инвариант bm25CountTerms (термы
// непустые, строго возрастают, tf ≥ 1): битый список тихо портил бы df/скоринг,
// лучше actionable-ошибка (defense-in-depth поверх CRC WAL/снапшота).
func readTerms(r io.Reader) ([]TermTF, error) {
	n, err := getU32(r)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]TermTF, 0, n)
	prev := ""
	var u2 [2]byte
	for i := uint32(0); i < n; i++ {
		term, err := readStr(r)
		if err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, u2[:]); err != nil {
			return nil, err
		}
		tf := uint16(u2[0]) | uint16(u2[1])<<8
		if term == "" || tf == 0 {
			return nil, fmt.Errorf("terms: пустой терм или tf=0 на позиции %d", i)
		}
		if i > 0 && term <= prev {
			return nil, fmt.Errorf("terms: нарушен порядок/уникальность (%q после %q)", term, prev)
		}
		prev = term
		out = append(out, TermTF{Term: term, TF: tf})
	}
	return out, nil
}

// writeSegText сериализует текстовый слой frozen/frozenSQ-сегмента (nil → 0B present).
func writeSegText(w io.Writer, st *segmentText) error {
	if st == nil {
		_, err := w.Write([]byte{0})
		return err
	}
	if _, err := w.Write([]byte{1}); err != nil {
		return err
	}
	if err := putU32(w, uint32(len(st.docLen))); err != nil {
		return err
	}
	for _, dl := range st.docLen {
		if err := putU32(w, dl); err != nil {
			return err
		}
	}
	if err := putU32(w, uint32(len(st.postings))); err != nil {
		return err
	}
	var u2 [2]byte
	for id, pl := range st.postings {
		if err := writeStr(w, st.dict.vals[id]); err != nil {
			return err
		}
		if err := putU32(w, uint32(len(pl))); err != nil {
			return err
		}
		for _, p := range pl {
			if err := putU32(w, p.doc); err != nil {
				return err
			}
			u2[0] = byte(p.tf)
			u2[1] = byte(p.tf >> 8)
			if _, err := w.Write(u2[:]); err != nil {
				return err
			}
		}
	}
	return nil
}

// readSegText — обратная операция. Словарь восстанавливается интернированием в
// порядке termID (термID = позиция записи — тот же id, что при build), счётчики
// nText/totalLen пересчитываются из docLen. Валидация инвариантов слоя
// (doc в границах, лист строго возрастает по doc) — битый слой не публикуем.
func readSegText(r io.Reader) (*segmentText, error) {
	var present [1]byte
	if _, err := io.ReadFull(r, present[:]); err != nil {
		return nil, err
	}
	if present[0] == 0 {
		return nil, nil
	}
	nDocs, err := getU32(r)
	if err != nil {
		return nil, err
	}
	st := &segmentText{
		dict:   newAttrDict(),
		docLen: make([]uint32, nDocs),
	}
	for i := range st.docLen {
		dl, err := getU32(r)
		if err != nil {
			return nil, err
		}
		st.docLen[i] = dl
		if dl > 0 {
			st.nText++
			st.totalLen += uint64(dl)
		}
	}
	nTerms, err := getU32(r)
	if err != nil {
		return nil, err
	}
	var u2 [2]byte
	for id := uint32(0); id < nTerms; id++ {
		term, err := readStr(r)
		if err != nil {
			return nil, err
		}
		if term == "" {
			return nil, fmt.Errorf("segText: пустой терм id=%d", id)
		}
		if got := st.dict.intern(term); got != id {
			return nil, fmt.Errorf("segText: терм %q дублирован (id %d ≠ %d)", term, got, id)
		}
		nPost, err := getU32(r)
		if err != nil {
			return nil, err
		}
		if nPost == 0 {
			return nil, fmt.Errorf("segText: пустой постинг-лист терма %q (df врал бы)", term)
		}
		pl := make([]bm25Posting, 0, nPost)
		prev := int64(-1)
		for p := uint32(0); p < nPost; p++ {
			doc, err := getU32(r)
			if err != nil {
				return nil, err
			}
			if _, err := io.ReadFull(r, u2[:]); err != nil {
				return nil, err
			}
			tf := uint16(u2[0]) | uint16(u2[1])<<8
			if doc >= nDocs || int64(doc) <= prev || tf == 0 {
				return nil, fmt.Errorf("segText: битый постинг терма %q (doc=%d prev=%d tf=%d nDocs=%d)", term, doc, prev, tf, nDocs)
			}
			prev = int64(doc)
			pl = append(pl, bm25Posting{doc: doc, tf: tf})
		}
		st.postings = append(st.postings, pl)
	}
	return st, nil
}

// decodeTerms восстанавливает пер-доковые термы из постингов — обратный проход
// buildSegmentText (бакетирование по doc). Термы каждого дока отсортированы
// (проход по словарю не даёт лексикографию — сортируем вставкой ID→строка
// невыгодно, поэтому финальная сортировка пер-док). Используется hnsw-путём
// снапшота (сегмент на load перестраивается из entries) и merge (шаг 6).
func (st *segmentText) decodeTerms() [][]TermTF {
	if st == nil {
		return nil
	}
	out := make([][]TermTF, len(st.docLen))
	for id, pl := range st.postings {
		term := st.dict.vals[id]
		for _, p := range pl {
			out[p.doc] = append(out[p.doc], TermTF{Term: term, TF: p.tf})
		}
	}
	for doc := range out {
		if len(out[doc]) > 1 {
			sortTermTF(out[doc])
		}
	}
	return out
}

// sortTermTF сортирует термы дока по строке (инвариант bm25CountTerms).
func sortTermTF(tt []TermTF) {
	// Вставками: списки коротки (термы одного дока), без аллокаций компаратора.
	for i := 1; i < len(tt); i++ {
		for j := i; j > 0 && tt[j].Term < tt[j-1].Term; j-- {
			tt[j], tt[j-1] = tt[j-1], tt[j]
		}
	}
}
