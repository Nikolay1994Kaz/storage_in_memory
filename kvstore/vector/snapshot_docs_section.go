package vector

// Документная секция бинарного снапшота — наследие прежних версий.
//
// Формат v8 добавил её frozen-сегменту, v9 — SQ8- и hnsw-сегментам: после
// слоёв сегмента идёт [nGroups u32] и группы документов. Туда уезжали
// документы с категориальным атрибутом scope (факты слоя памяти агентов):
// в графе их векторы занулялись, из колонок и текстового слоя они изымались,
// а сама группа при -encrypt-at-rest шифровалась ключом скоупа.
//
// Слоя памяти агентов и шифрования больше нет. Формат v9 при этом не меняется:
// секция пишется ПУСТОЙ (nGroups = 0), поэтому новые снапшоты читаются и
// прежними версиями, а атрибут scope — обычный атрибут, как любой другой.
//
// Читается секция ради снапшотов прежних версий:
//   - открытая группа (документы без шифрования) восстанавливается целиком —
//     векторы вписываются обратно в граф, колонки и текст пересобираются;
//   - зашифрованная группа — отказ загрузки (ErrEncryptedSnapshot). Ключей у
//     этой версии нет, а загрузить снапшот без части документов значило бы
//     молча потерять данные.
//
// Формат группы:
//
//	[sealed 1B][nDocs u32]
//	nDocs × [pos u32][keyLen u16][key]
//	[payloadLen u32][payload]
//	payload = nDocs × [blobLen u32][blob], blob = SerializeVectorWithDoc
//	(у зашифрованной группы payload — конверт кейринга)

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrEncryptedSnapshot — снапшот прежней версии содержит документы,
// зашифрованные при -encrypt-at-rest. Эта версия их прочитать не может.
var ErrEncryptedSnapshot = errors.New("snapshot holds documents encrypted at rest by an older version (-encrypt-at-rest); this version cannot read them")

const (
	docsGroupPlain     byte = 0 // документы открытым текстом
	docsGroupEncrypted byte = 1 // группа под ключом скоупа
)

// writeEmptyDocsSection пишет пустую документную секцию: формат v9 её
// требует, документов в ней больше не бывает.
func writeEmptyDocsSection(w io.Writer) error {
	return putU32(w, 0)
}

// readDocsSection читает секцию и возвращает документы, выровненные по
// позициям сегмента (длина n; позиции без документа — нулевые DeltaEntry).
func readDocsSection(r io.Reader, n int) ([]DeltaEntry, error) {
	nGroups, err := getU32(r)
	if err != nil {
		return nil, fmt.Errorf("docs section: read nGroups: %w", err)
	}
	if nGroups == 0 {
		return nil, nil
	}
	// Группа на диске: маркер, nDocs и длина payload — не меньше 9 байт.
	if err := needBytes(r, int64(nGroups), 9); err != nil {
		return nil, fmt.Errorf("docs section: %w", err)
	}
	entries := make([]DeltaEntry, n)

	for g := uint32(0); g < nGroups; g++ {
		var marker [1]byte
		if _, err := io.ReadFull(r, marker[:]); err != nil {
			return nil, fmt.Errorf("docs section: read marker[%d]: %w", g, err)
		}
		if marker[0] == docsGroupEncrypted {
			return nil, ErrEncryptedSnapshot
		}
		if marker[0] != docsGroupPlain {
			return nil, fmt.Errorf("docs section: unknown group marker %d", marker[0])
		}
		nDocs, err := getU32(r)
		if err != nil {
			return nil, fmt.Errorf("docs section: read nDocs[%d]: %w", g, err)
		}
		// Документ на диске: позиция и длина ключа — не меньше 8 байт.
		if err := needBytes(r, int64(nDocs), 8); err != nil {
			return nil, fmt.Errorf("docs section: %w", err)
		}

		positions := make([]int, 0, nDocs)
		keys := make([]string, 0, nDocs)
		for d := uint32(0); d < nDocs; d++ {
			pos, err := getU32(r)
			if err != nil {
				return nil, fmt.Errorf("docs section: read pos: %w", err)
			}
			key, err := readStr(r)
			if err != nil {
				return nil, fmt.Errorf("docs section: read key: %w", err)
			}
			if int(pos) >= n {
				return nil, fmt.Errorf("docs section: position %d outside segment (n=%d)", pos, n)
			}
			positions = append(positions, int(pos))
			keys = append(keys, key)
		}

		payloadLen, err := getU32(r)
		if err != nil {
			return nil, fmt.Errorf("docs section: read payloadLen: %w", err)
		}
		if err := needBytes(r, int64(payloadLen), 1); err != nil {
			return nil, fmt.Errorf("docs section: %w", err)
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("docs section: read payload: %w", err)
		}

		off := 0
		for i, pos := range positions {
			if off+4 > len(payload) {
				return nil, fmt.Errorf("docs section: payload truncated at doc %d", i)
			}
			blobLen := int(binary.LittleEndian.Uint32(payload[off : off+4]))
			off += 4
			if off+blobLen > len(payload) {
				return nil, fmt.Errorf("docs section: blob %d overruns payload", i)
			}
			vec, attrs, terms, err := DeserializeVectorWithDoc(payload[off : off+blobLen])
			if err != nil {
				return nil, fmt.Errorf("docs section: decode doc %d: %w", i, err)
			}
			off += blobLen
			entries[pos] = DeltaEntry{Key: keys[i], Vec: vec, Attrs: attrs, Terms: terms}
		}
	}
	return entries, nil
}

// hasAnyDoc — есть ли в наборе хоть один документ. Пустой набор означает,
// что секция была пустой и пересобирать слои сегмента не нужно.
func hasAnyDoc(entries []DeltaEntry) bool {
	for i := range entries {
		if entries[i].Key != "" {
			return true
		}
	}
	return false
}

// mergeDocsIntoEntries вписывает документы секции в набор документов сегмента
// по их позициям. Из объединённого набора пересобираются колонки и текст.
func mergeDocsIntoEntries(segment, docs []DeltaEntry) []DeltaEntry {
	out := make([]DeltaEntry, len(segment))
	copy(out, segment)
	for i := range docs {
		if i >= len(out) || docs[i].Key == "" {
			continue
		}
		out[i] = docs[i]
	}
	return out
}

// frozenEntries разворачивает frozen-сегмент в документы тем же способом, что
// и merge: вектор из слэба по позиции, атрибуты через decodeAt, термы через
// decodeTerms. Дыры (удалённые ключи) остаются дырами: все слои сегмента
// адресуются позицией.
func frozenEntries(s *frozenSegment) []DeltaEntry {
	fg := s.fg
	decTerms := s.text.decodeTerms()
	entries := make([]DeltaEntry, fg.n)
	for i := 0; i < fg.n; i++ {
		if fg.keys.view(i) == "" {
			continue
		}
		var terms []TermTF
		if i < len(decTerms) {
			terms = decTerms[i]
		}
		entries[i] = DeltaEntry{
			Key:   fg.keys.clone(i),
			Vec:   fg.data[i*fg.dim : (i+1)*fg.dim],
			Attrs: s.attrs.decodeAt(i),
			Terms: terms,
		}
	}
	return entries
}

// frozenSQEntries — то же для SQ8-сегмента; вектор деквантуется.
func frozenSQEntries(s *frozenSQSegment) []DeltaEntry {
	fg := s.fg
	decTerms := s.text.decodeTerms()
	entries := make([]DeltaEntry, fg.n)
	for i := 0; i < fg.n; i++ {
		if fg.keys.view(i) == "" {
			continue
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

// restoreDocVectors вписывает векторы документов секции обратно в слэб графа:
// на диске на их местах лежали нули.
func restoreDocVectors(fg *FrozenGraph, docs []DeltaEntry) {
	for i, e := range docs {
		if e.Key == "" || len(e.Vec) != fg.dim || i >= fg.n {
			continue
		}
		copy(fg.data[i*fg.dim:(i+1)*fg.dim], e.Vec)
	}
}

// restoreDocVectorsSQ квантует векторы документов секции обратно в коды
// калибровкой сегмента — дословно как FreezeGraphSQ, включая константную
// размерность (sqScale == 0 → код 0) и округление к ближайшему. В секции лежат
// деквантованные коды, поэтому round-trip восстанавливает их бит в бит.
func restoreDocVectorsSQ(fg *FrozenGraphSQ, docs []DeltaEntry) {
	for i, e := range docs {
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
