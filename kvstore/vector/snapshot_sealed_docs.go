package vector

// Документная секция бинарного снапшота: то, чем закрывается вторая половина
// криптостирания.
//
// ПОЧЕМУ НЕ ПРОЩЕ. Слои сегмента — граф, колонки атрибутов, инвертированный
// BM25-индекс — индексируются ПОЗИЦИЕЙ документа, и скоупы в них перемешаны:
// сегменты нарезаны по времени вставки, а не по владельцу. Зашифровать часть
// такого сегмента нельзя даже теоретически — рёбра графа указывали бы в
// шифротекст, а постинг-лист терма содержит документы разных людей. Зато
// ДОКУМЕНТНАЯ форма (DeltaEntry) делится по скоупу тривиально, а обратно в
// слои собирается тем же кодом, которым её собирает флаш дельты
// (buildSegmentAttrs/buildSegmentText). Поэтому шифруется то, что делимо, и
// пересобирается то, что нет. Цена измерена ДО кода: 18–20 мс на 10k фактов
// (snapshot_rebuild_cost_bench_test), против 33–76 мс нынешней загрузки.
//
// ЧТО ОСТАЁТСЯ ОТКРЫТЫМ И ПОЧЕМУ. Ключи (ULID) и позиции лежат вне конверта.
// Иначе мёртвый документ нечем пометить: tombstones живут по КЛЮЧУ, и позиция
// с пустым ключом всплыла бы в выдаче. Тень, которая при этом остаётся, —
// «существовал факт с таким id в такой момент»: без скоупа (он внутри
// конверта), без содержания, и kek_id после уничтожения ключа ни с чем не
// связывается, потому что запись в кейринге переписана.

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// SnapshotCrypto — точка подмены шифрования для снапшота. Пакет vector
// НАМЕРЕННО не знает про internal/keyring: слоистость та же, что у sealValue
// в main, и по той же причине — движок работает с открытым текстом, а
// шифрование живёт на границе персистентности.
//
// Unseal отделяет «ключ уничтожен» от «данные битые» третьим значением, а не
// ошибкой: первое — ШТАТНЫЙ исход криптостирания, о котором надо молча
// продолжить работу, второе — порча, о которой молчать нельзя.
type SnapshotCrypto struct {
	Seal   func(scope string, plain []byte) ([]byte, error)
	Unseal func(envelope []byte) (plain []byte, destroyed bool, err error)
}

// sealedGroupHeader — маркеры группы документов.
const (
	sealedGroupPlain  byte = 0 // документы без скоупа: шифровать нечем
	sealedGroupSealed byte = 1 // группа одного скоупа под его ключом
)

// docsGroupedByScope раскладывает документы по скоупу, сохраняя позиции.
// Документы без скоупа (не VMEM-факты) собираются в одну открытую группу:
// у них нет ключа, под которым их потом откроют.
//
// Порядок групп детерминирован (по имени скоупа), потому что снапшот обязан
// быть воспроизводимым: иначе два снапшота одного состояния отличались бы
// байтами, и любая проверка целостности стала бы шумом.
func docsGroupedByScope(entries []DeltaEntry, scopeOf func(DeltaEntry) string) ([]string, map[string][]int) {
	byScope := make(map[string][]int)
	for i, e := range entries {
		if e.Key == "" {
			continue // дыра в сегменте (tombstone/удалённый) — документа нет
		}
		byScope[scopeOf(e)] = append(byScope[scopeOf(e)], i)
	}
	order := make([]string, 0, len(byScope))
	for scope := range byScope {
		order = append(order, scope)
	}
	sort.Strings(order)
	return order, byScope
}

// writeSealedDocs сериализует документы сегмента группами по скоупу.
//
// Формат группы:
//
//	[sealed 1B][nDocs u32]
//	nDocs × [pos u32][keyLen u16][key]     — ВСЕГДА открыто (см. шапку файла)
//	[payloadLen u32][payload]              — конверт скоупа либо сырьё
//	payload = nDocs × [blobLen u32][blob], blob = SerializeVectorWithDoc
//
// crypto == nil или отсутствие Seal → все группы пишутся открытыми: движок без
// -encrypt-at-rest ведёт себя ровно как раньше.
func writeSealedDocs(w io.Writer, entries []DeltaEntry, scopeOf func(DeltaEntry) string, crypto *SnapshotCrypto) error {
	order, byScope := docsGroupedByScope(entries, scopeOf)

	if err := putU32(w, uint32(len(order))); err != nil {
		return fmt.Errorf("sealed docs: write nGroups: %w", err)
	}
	for _, scope := range order {
		positions := byScope[scope]
		sealed := scope != "" && crypto != nil && crypto.Seal != nil

		var marker [1]byte
		if sealed {
			marker[0] = sealedGroupSealed
		} else {
			marker[0] = sealedGroupPlain
		}
		if _, err := w.Write(marker[:]); err != nil {
			return fmt.Errorf("sealed docs: write marker: %w", err)
		}
		if err := putU32(w, uint32(len(positions))); err != nil {
			return fmt.Errorf("sealed docs: write nDocs: %w", err)
		}

		var payload []byte
		for _, pos := range positions {
			e := entries[pos]
			if err := putU32(w, uint32(pos)); err != nil {
				return fmt.Errorf("sealed docs: write pos: %w", err)
			}
			if err := writeStr(w, e.Key); err != nil {
				return fmt.Errorf("sealed docs: write key: %w", err)
			}
			blob := SerializeVectorWithDoc(e.Vec, e.Attrs, e.Terms)
			var lenBuf [4]byte
			binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(blob)))
			payload = append(payload, lenBuf[:]...)
			payload = append(payload, blob...)
		}

		if sealed {
			envelope, err := crypto.Seal(scope, payload)
			if err != nil {
				// Тихо писать открытым текстом «на всякий случай» нельзя: это
				// ровно та ложь, против которой весь механизм.
				return fmt.Errorf("sealed docs: seal scope %q: %w", scope, err)
			}
			payload = envelope
		}
		if err := putU32(w, uint32(len(payload))); err != nil {
			return fmt.Errorf("sealed docs: write payloadLen: %w", err)
		}
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("sealed docs: write payload: %w", err)
		}
	}
	return nil
}

// readSealedDocs — обратная операция. Возвращает документы, выровненные по
// позициям (длина n, дыры — нулевые DeltaEntry), и ключи документов, чья
// группа не открылась из-за уничтоженного ключа: их место в графе остаётся, но
// сами они мертвы, и вызывающий обязан положить их в tombstones.
func readSealedDocs(r io.Reader, n int, crypto *SnapshotCrypto) (entries []DeltaEntry, dead []string, err error) {
	nGroups, err := getU32(r)
	if err != nil {
		return nil, nil, fmt.Errorf("sealed docs: read nGroups: %w", err)
	}
	entries = make([]DeltaEntry, n)

	for g := uint32(0); g < nGroups; g++ {
		var marker [1]byte
		if _, err := io.ReadFull(r, marker[:]); err != nil {
			return nil, nil, fmt.Errorf("sealed docs: read marker[%d]: %w", g, err)
		}
		nDocs, err := getU32(r)
		if err != nil {
			return nil, nil, fmt.Errorf("sealed docs: read nDocs[%d]: %w", g, err)
		}

		positions := make([]int, 0, nDocs)
		keys := make([]string, 0, nDocs)
		for d := uint32(0); d < nDocs; d++ {
			pos, err := getU32(r)
			if err != nil {
				return nil, nil, fmt.Errorf("sealed docs: read pos: %w", err)
			}
			key, err := readStr(r)
			if err != nil {
				return nil, nil, fmt.Errorf("sealed docs: read key: %w", err)
			}
			if int(pos) >= n {
				return nil, nil, fmt.Errorf("sealed docs: позиция %d вне сегмента (n=%d)", pos, n)
			}
			positions = append(positions, int(pos))
			keys = append(keys, key)
		}

		payloadLen, err := getU32(r)
		if err != nil {
			return nil, nil, fmt.Errorf("sealed docs: read payloadLen: %w", err)
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, nil, fmt.Errorf("sealed docs: read payload: %w", err)
		}

		if marker[0] == sealedGroupSealed {
			if crypto == nil || crypto.Unseal == nil {
				// Данные под ключом, а кейринга нет. Это не «пропустить и
				// продолжить»: без кейринга снапшот прочитан НЕПОЛНО, и
				// молчание превратило бы потерю данных в тихую деградацию.
				return nil, nil, fmt.Errorf("sealed docs: группа запечатана, но кейринг не подключён")
			}
			plain, destroyed, err := crypto.Unseal(payload)
			if destroyed {
				// ШТАТНЫЙ исход криптостирания: скоуп стёрт. Документы не
				// восстанавливаем, их ключи отдаём в tombstones.
				dead = append(dead, keys...)
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("sealed docs: unseal: %w", err)
			}
			payload = plain
		}

		off := 0
		for i, pos := range positions {
			if off+4 > len(payload) {
				return nil, nil, fmt.Errorf("sealed docs: payload оборван на документе %d", i)
			}
			blobLen := int(binary.LittleEndian.Uint32(payload[off : off+4]))
			off += 4
			if off+blobLen > len(payload) {
				return nil, nil, fmt.Errorf("sealed docs: blob %d выходит за payload", i)
			}
			vec, attrs, terms, err := DeserializeVectorWithDoc(payload[off : off+blobLen])
			if err != nil {
				return nil, nil, fmt.Errorf("sealed docs: decode doc %d: %w", i, err)
			}
			off += blobLen
			entries[pos] = DeltaEntry{Key: keys[i], Vec: vec, Attrs: attrs, Terms: terms}
		}
	}
	return entries, dead, nil
}
