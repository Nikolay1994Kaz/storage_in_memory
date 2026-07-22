package vector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// =============================================================================
// VMEM шаг 7 — durability пары supersedes (docs/VMEM_DESIGN.md).
//
// Пара «закрытая цель + наследник» обязана уехать в WAL одной атомарной
// записью (OpVSimAddDocBatch): полузаписанная пара после краша — это либо
// «два истинных сейчас» факта, либо «закрыт без наследника», и реплей честно
// воспроизвёл бы любую из этих лжей. Контейнер решает атомарность одним CRC.
//
// Формат Value: [uvarint n] затем n × ([uvarint keyLen][key][uvarint blobLen]
// [blob]), где blob = SerializeVectorWithDoc (формат дока НЕ дублируется —
// та же секция, что у OpVSimAddDoc).
// =============================================================================

// BatchDoc — один док атомарного WAL-батча.
type BatchDoc struct {
	Key   string
	Vec   []float32
	Attrs Attributes
	Terms []TermTF
}

// SerializeDocBatch кодирует доки в Value для OpVSimAddDocBatch.
func SerializeDocBatch(docs []BatchDoc) []byte {
	var buf bytes.Buffer
	var u [binary.MaxVarintLen64]byte
	put := func(v uint64) { buf.Write(u[:binary.PutUvarint(u[:], v)]) }
	put(uint64(len(docs)))
	for _, d := range docs {
		put(uint64(len(d.Key)))
		buf.WriteString(d.Key)
		blob := SerializeVectorWithDoc(d.Vec, d.Attrs, d.Terms)
		put(uint64(len(blob)))
		buf.Write(blob)
	}
	return buf.Bytes()
}

// DeserializeDocBatch — обратная операция для replay OpVSimAddDocBatch.
func DeserializeDocBatch(data []byte) ([]BatchDoc, error) {
	r := bytes.NewReader(data)
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("doc batch: read n: %w", err)
	}
	// Кап на n — защита от мусорного блоба (реалистичный батч — единицы доков).
	if n > 1<<20 {
		return nil, fmt.Errorf("doc batch: implausible n=%d", n)
	}
	docs := make([]BatchDoc, 0, n)
	for i := uint64(0); i < n; i++ {
		kl, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, fmt.Errorf("doc batch: doc[%d] keyLen: %w", i, err)
		}
		key := make([]byte, kl)
		if _, err := io.ReadFull(r, key); err != nil {
			return nil, fmt.Errorf("doc batch: doc[%d] key: %w", i, err)
		}
		bl, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, fmt.Errorf("doc batch: doc[%d] blobLen: %w", i, err)
		}
		blob := make([]byte, bl)
		if _, err := io.ReadFull(r, blob); err != nil {
			return nil, fmt.Errorf("doc batch: doc[%d] blob: %w", i, err)
		}
		vec, attrs, terms, err := DeserializeVectorWithDoc(blob)
		if err != nil {
			return nil, fmt.Errorf("doc batch: doc[%d]: %w", i, err)
		}
		docs = append(docs, BatchDoc{Key: string(key), Vec: vec, Attrs: attrs, Terms: terms})
	}
	return docs, nil
}
