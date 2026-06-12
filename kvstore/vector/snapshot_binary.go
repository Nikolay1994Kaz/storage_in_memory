package vector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"unsafe"
)

// Magic header и версия для проверки валидности файла
var magicHeader = []byte("HNSW\x00\x01\x00\x00")

// SaveBinary сериализует весь HNSW-граф в высокопроизводительный бинарный формат.
// Использует unsafe.Slice для копирования сырых блоков памяти без лишних аллокаций.
func (vs *VectorStore) SaveBinary(w io.Writer) error {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	// 1. Записываем все секции во временный буфер в оперативной памяти
	var buf bytes.Buffer

	// === SECTION 1: Metadata (ID=1) ===
	if err := writeSectionMetadata(&buf, vs); err != nil {
		return fmt.Errorf("metadata write: %w", err)
	}

	// === SECTION 2: Nodes (ID=2) ===
	if err := writeSectionNodes(&buf, vs.graph); err != nil {
		return fmt.Errorf("nodes write: %w", err)
	}

	// === SECTION 3: FreeIDs (ID=3) ===
	if err := writeSectionFreeIDs(&buf, vs.graph); err != nil {
		return fmt.Errorf("freeIDs write: %w", err)
	}

	// === SECTION 4: VectorArena (ID=4) ===
	if err := writeSectionVectorArena(&buf, vs.graph.arena); err != nil {
		return fmt.Errorf("vector arena write: %w", err)
	}

	// === SECTION 5: Neighbors (ID=5) ===
	if err := writeSectionNeighbors(&buf, vs.graph); err != nil {
		return fmt.Errorf("neighbors write: %w", err)
	}

	// === SECTION 6: KeyMap (ID=6) ===
	if err := writeSectionKeyMap(&buf, vs); err != nil {
		return fmt.Errorf("key map write: %w", err)
	}

	// 2. Вычисляем CRC32 и общий размер полезной нагрузки
	payloadBytes := buf.Bytes()
	checksum := crc32.ChecksumIEEE(payloadBytes)
	totalSize := uint64(len(payloadBytes))

	// 3. Пишем заголовок в результирующий поток w
	// Magic сигнатура: 8 байт
	if _, err := w.Write(magicHeader); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}

	// CRC32 чексумма: 4 байта
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], checksum)
	if _, err := w.Write(crcBuf[:]); err != nil {
		return fmt.Errorf("write checksum: %w", err)
	}

	// Полный размер данных: 8 байт
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], totalSize)
	if _, err := w.Write(sizeBuf[:]); err != nil {
		return fmt.Errorf("write total size: %w", err)
	}

	// 4. Записываем саму полезную нагрузку (все секции)
	if _, err := w.Write(payloadBytes); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}

	return nil
}

// LoadBinary восстанавливает состояние графа из бинарного snapshot за один проход.
func (vs *VectorStore) LoadBinary(r io.Reader) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	// 1. Проверяем Magic сигнатуру
	magic := make([]byte, len(magicHeader))
	if _, err := io.ReadFull(r, magic); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if !bytes.Equal(magic, magicHeader) {
		return fmt.Errorf("magic header mismatch: invalid file version or format")
	}

	// 2. Читаем CRC32 чексумму
	var crcBuf [4]byte
	if _, err := io.ReadFull(r, crcBuf[:]); err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	expectedChecksum := binary.LittleEndian.Uint32(crcBuf[:])

	// 3. Читаем полный размер полезной нагрузки
	var sizeBuf [8]byte
	if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
		return fmt.Errorf("read total size: %w", err)
	}
	totalSize := binary.LittleEndian.Uint64(sizeBuf[:])

	// 4. Считываем всю полезную нагрузку в память
	payloadBytes := make([]byte, totalSize)
	if _, err := io.ReadFull(r, payloadBytes); err != nil {
		return fmt.Errorf("read payload (size %d): %w", totalSize, err)
	}

	// 5. Проверяем контрольную сумму CRC32
	actualChecksum := crc32.ChecksumIEEE(payloadBytes)
	if actualChecksum != expectedChecksum {
		return fmt.Errorf("CRC32 mismatch: data corruption detected (expected %08x, got %08x)", expectedChecksum, actualChecksum)
	}

	// 6. Инициализируем структуры графа под восстановление
	vs.keys = make(map[uint64]string)
	vs.ids = make(map[string]uint64)
	vs.graph = &Graph{
		allocator:     vs.allocator,
		pruneBufItems: make([]item, 0, 33),
		pruneBufIDs:   make([]uint64, 0, 33),
		insertBuf:     make([]uint64, 0, 33),
	}

	// 7. Посекционный парсинг из буфера в памяти
	reader := bytes.NewReader(payloadBytes)
	for reader.Len() > 0 {
		sectionIDByte, err := reader.ReadByte()
		if err != nil {
			return fmt.Errorf("read section ID: %w", err)
		}
		sectionID := int(sectionIDByte)

		var secLenBuf [4]byte
		if _, err := io.ReadFull(reader, secLenBuf[:]); err != nil {
			return fmt.Errorf("read section length: %w", err)
		}
		sectionLen := binary.LittleEndian.Uint32(secLenBuf[:])

		sectionPayload := make([]byte, sectionLen)
		if _, err := io.ReadFull(reader, sectionPayload); err != nil {
			return fmt.Errorf("read section payload (size %d): %w", sectionLen, err)
		}

		secReader := bytes.NewReader(sectionPayload)
		switch sectionID {
		case 1:
			if err := readSectionMetadata(secReader, vs); err != nil {
				return fmt.Errorf("parse metadata: %w", err)
			}
		case 2:
			if err := readSectionNodes(secReader, vs.graph); err != nil {
				return fmt.Errorf("parse nodes: %w", err)
			}
		case 3:
			if err := readSectionFreeIDs(secReader, vs.graph); err != nil {
				return fmt.Errorf("parse freeIDs: %w", err)
			}
		case 4:
			if err := readSectionVectorArena(secReader, vs.graph); err != nil {
				return fmt.Errorf("parse vector arena: %w", err)
			}
		case 5:
			if err := readSectionNeighbors(secReader, vs.graph); err != nil {
				return fmt.Errorf("parse neighbors: %w", err)
			}
		case 6:
			if err := readSectionKeyMap(secReader, vs); err != nil {
				return fmt.Errorf("parse key map: %w", err)
			}
		}
	}

	// Обновляем размерность во вложенной VectorArena
	if vs.graph.arena != nil {
		vs.graph.arena.dim = vs.dim
	}

	// Перестраиваем LSH индекс, если размерность высокая (dim >= 256)
	if vs.dim >= 256 {
		vs.lsh = NewLSHIndex(vs.dim, 42)
		// Пре-аллоцируем hashes до размера len(vs.graph.nodes)
		vs.lsh.hashes = make([]uint64, len(vs.graph.nodes))
		for i := range vs.lsh.hashes {
			vs.lsh.hashes[i] = lshSentinel
		}
		for id, node := range vs.graph.nodes {
			if node.Alive {
				vec := vs.graph.arena.Get(node.VectorOffset)
				vs.lsh.Insert(uint32(id), vec)
			}
		}
	} else {
		vs.lsh = nil
	}

	return nil
}

// ============================================================================
// Вспомогательные функции записи (Serialization Helpers)
// ============================================================================

func writeSectionMetadata(w io.Writer, vs *VectorStore) error {
	g := vs.graph
	var data [45]byte
	binary.LittleEndian.PutUint32(data[0:4], uint32(g.M))
	binary.LittleEndian.PutUint32(data[4:8], uint32(g.M0))
	binary.LittleEndian.PutUint64(data[8:16], math.Float64bits(g.Ml))
	binary.LittleEndian.PutUint32(data[16:20], uint32(g.EfConstruction))
	binary.LittleEndian.PutUint32(data[20:24], uint32(vs.dim))
	binary.LittleEndian.PutUint32(data[24:28], uint32(g.nodeCount))
	binary.LittleEndian.PutUint32(data[28:32], uint32(len(g.nodes)))
	binary.LittleEndian.PutUint32(data[32:36], uint32(g.maxLevel))
	binary.LittleEndian.PutUint64(data[36:44], g.entryPointID)
	if vs.autoNormalize {
		data[44] = 1
	} else {
		data[44] = 0
	}

	if _, err := w.Write([]byte{1}); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], 45)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(data[:])
	return err
}

func writeSectionNodes(w io.Writer, g *Graph) error {
	count := uint32(len(g.nodes))
	nodeSize := int(unsafe.Sizeof(Node{}))
	sectionSize := 4 + count*uint32(nodeSize)

	if _, err := w.Write([]byte{2}); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], sectionSize)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	var countBuf [4]byte
	binary.LittleEndian.PutUint32(countBuf[:], count)
	if _, err := w.Write(countBuf[:]); err != nil {
		return err
	}

	if count > 0 {
		sizeInBytes := count * uint32(nodeSize)
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&g.nodes[0])), sizeInBytes)
		if _, err := w.Write(byteSlice); err != nil {
			return err
		}
	}
	return nil
}

func writeSectionFreeIDs(w io.Writer, g *Graph) error {
	count := uint32(len(g.freeIDs))
	sectionSize := 4 + count*4

	if _, err := w.Write([]byte{3}); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], sectionSize)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	var countBuf [4]byte
	binary.LittleEndian.PutUint32(countBuf[:], count)
	if _, err := w.Write(countBuf[:]); err != nil {
		return err
	}

	if count > 0 {
		sizeInBytes := count * 4
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&g.freeIDs[0])), sizeInBytes)
		if _, err := w.Write(byteSlice); err != nil {
			return err
		}
	}
	return nil
}

func writeSectionVectorArena(w io.Writer, va *VectorArena) error {
	if va == nil {
		if _, err := w.Write([]byte{4}); err != nil {
			return err
		}
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], 8)
		if _, err := w.Write(lenBuf[:]); err != nil {
			return err
		}
		var zeroes [8]byte
		_, err := w.Write(zeroes[:])
		return err
	}

	dataLen := uint32(len(va.data))
	freeCount := uint32(len(va.freeOffsets))
	sectionSize := 4 + dataLen*4 + 4 + freeCount*4

	if _, err := w.Write([]byte{4}); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], sectionSize)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	var dataLenBuf [4]byte
	binary.LittleEndian.PutUint32(dataLenBuf[:], dataLen)
	if _, err := w.Write(dataLenBuf[:]); err != nil {
		return err
	}
	if dataLen > 0 {
		sizeInBytes := dataLen * 4
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&va.data[0])), sizeInBytes)
		if _, err := w.Write(byteSlice); err != nil {
			return err
		}
	}
	var freeCountBuf [4]byte
	binary.LittleEndian.PutUint32(freeCountBuf[:], freeCount)
	if _, err := w.Write(freeCountBuf[:]); err != nil {
		return err
	}
	if freeCount > 0 {
		sizeInBytes := freeCount * 4
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&va.freeOffsets[0])), sizeInBytes)
		if _, err := w.Write(byteSlice); err != nil {
			return err
		}
	}
	return nil
}

func writeSectionNeighbors(w io.Writer, g *Graph) error {
	count := uint32(len(g.nodes))
	
	// Сначала посчитаем размер секции
	sectionSize := uint32(4) // count
	for i := uint32(0); i < count; i++ {
		node := &g.nodes[i]
		sectionSize += 4 // level
		if node.Alive {
			for level := 0; level <= node.Level; level++ {
				neighbors := g.getNeighbors(node.NeighborsHandle, level)
				sectionSize += 4 + uint32(len(neighbors))*8
			}
		} else {
			// Для неживых нод пишем 0 соседей
			sectionSize += 4 // numNeighbors = 0
		}
	}

	if _, err := w.Write([]byte{5}); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], sectionSize)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}

	var countBuf [4]byte
	binary.LittleEndian.PutUint32(countBuf[:], count)
	if _, err := w.Write(countBuf[:]); err != nil {
		return err
	}

	for i := uint32(0); i < count; i++ {
		node := &g.nodes[i]
		var lvlBuf [4]byte
		if node.Alive {
			binary.LittleEndian.PutUint32(lvlBuf[:], uint32(node.Level))
			if _, err := w.Write(lvlBuf[:]); err != nil {
				return err
			}
			for level := 0; level <= node.Level; level++ {
				neighbors := g.getNeighbors(node.NeighborsHandle, level)
				var numBuf [4]byte
				binary.LittleEndian.PutUint32(numBuf[:], uint32(len(neighbors)))
				if _, err := w.Write(numBuf[:]); err != nil {
					return err
				}
				if len(neighbors) > 0 {
					sizeInBytes := uint32(len(neighbors)) * 8
					byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&neighbors[0])), sizeInBytes)
					if _, err := w.Write(byteSlice); err != nil {
						return err
					}
				}
			}
		} else {
			binary.LittleEndian.PutUint32(lvlBuf[:], 0)
			if _, err := w.Write(lvlBuf[:]); err != nil {
				return err
			}
			var numBuf [4]byte
			binary.LittleEndian.PutUint32(numBuf[:], 0)
			if _, err := w.Write(numBuf[:]); err != nil {
				return err
			}
		}
	}

	return nil
}

func writeSectionKeyMap(w io.Writer, vs *VectorStore) error {
	count := uint32(len(vs.keys))
	mapSize := uint32(0)
	for _, key := range vs.keys {
		mapSize += 8 + 2 + uint32(len(key))
	}
	sectionSize := 4 + mapSize

	if _, err := w.Write([]byte{6}); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], sectionSize)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	var countBuf [4]byte
	binary.LittleEndian.PutUint32(countBuf[:], count)
	if _, err := w.Write(countBuf[:]); err != nil {
		return err
	}

	for id, key := range vs.keys {
		var idBuf [8]byte
		binary.LittleEndian.PutUint64(idBuf[:], id)
		if _, err := w.Write(idBuf[:]); err != nil {
			return err
		}
		keyLen := uint16(len(key))
		var keyLenBuf [2]byte
		binary.LittleEndian.PutUint16(keyLenBuf[:], keyLen)
		if _, err := w.Write(keyLenBuf[:]); err != nil {
			return err
		}
		if keyLen > 0 {
			if _, err := w.Write([]byte(key)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ============================================================================
// Вспомогательные функции чтения (Deserialization Helpers)
// ============================================================================

func readSectionMetadata(r io.Reader, vs *VectorStore) error {
	var data [45]byte
	if _, err := io.ReadFull(r, data[:]); err != nil {
		return err
	}

	g := vs.graph
	g.M = int(binary.LittleEndian.Uint32(data[0:4]))
	g.M0 = int(binary.LittleEndian.Uint32(data[4:8]))
	g.Ml = math.Float64frombits(binary.LittleEndian.Uint64(data[8:16]))
	g.EfConstruction = int(binary.LittleEndian.Uint32(data[16:20]))
	vs.dim = int(binary.LittleEndian.Uint32(data[20:24]))
	g.nodeCount = int(binary.LittleEndian.Uint32(data[24:28]))
	nodesLen := binary.LittleEndian.Uint32(data[28:32])
	g.maxLevel = int(binary.LittleEndian.Uint32(data[32:36]))
	g.entryPointID = binary.LittleEndian.Uint64(data[36:44])
	vs.autoNormalize = data[44] == 1

	if vs.autoNormalize {
		g.Distance = DotProductDistance
	} else {
		g.Distance = EuclideanDistance
	}

	g.pruneBufItems = make([]item, 0, g.M0+1)
	g.pruneBufIDs = make([]uint64, 0, g.M0+1)
	g.insertBuf = make([]uint64, 0, g.M0+1)
	g.nodes = make([]Node, nodesLen)

	return nil
}

func readSectionNodes(r io.Reader, g *Graph) error {
	var countBuf [4]byte
	if _, err := io.ReadFull(r, countBuf[:]); err != nil {
		return err
	}
	count := binary.LittleEndian.Uint32(countBuf[:])

	if count != uint32(len(g.nodes)) {
		return fmt.Errorf("nodes count mismatch in header vs metadata")
	}

	if count > 0 {
		nodeSize := int(unsafe.Sizeof(Node{}))
		sizeInBytes := count * uint32(nodeSize)
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&g.nodes[0])), sizeInBytes)
		if _, err := io.ReadFull(r, byteSlice); err != nil {
			return err
		}
	}
	return nil
}

func readSectionFreeIDs(r io.Reader, g *Graph) error {
	var countBuf [4]byte
	if _, err := io.ReadFull(r, countBuf[:]); err != nil {
		return err
	}
	count := binary.LittleEndian.Uint32(countBuf[:])

	g.freeIDs = make([]uint32, count)
	if count > 0 {
		sizeInBytes := count * 4
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&g.freeIDs[0])), sizeInBytes)
		if _, err := io.ReadFull(r, byteSlice); err != nil {
			return err
		}
	}
	return nil
}

func readSectionVectorArena(r io.Reader, g *Graph) error {
	var dataLenBuf [4]byte
	if _, err := io.ReadFull(r, dataLenBuf[:]); err != nil {
		return err
	}
	dataLen := binary.LittleEndian.Uint32(dataLenBuf[:])

	va := &VectorArena{
		dim: g.M,
	}
	g.arena = va

	va.data = make([]float32, dataLen)
	if dataLen > 0 {
		sizeInBytes := dataLen * 4
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&va.data[0])), sizeInBytes)
		if _, err := io.ReadFull(r, byteSlice); err != nil {
			return err
		}
	}

	var freeCountBuf [4]byte
	if _, err := io.ReadFull(r, freeCountBuf[:]); err != nil {
		return err
	}
	freeCount := binary.LittleEndian.Uint32(freeCountBuf[:])

	va.freeOffsets = make([]uint32, freeCount)
	if freeCount > 0 {
		sizeInBytes := freeCount * 4
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&va.freeOffsets[0])), sizeInBytes)
		if _, err := io.ReadFull(r, byteSlice); err != nil {
			return err
		}
	}

	return nil
}

func readSectionNeighbors(r io.Reader, g *Graph) error {
	var countBuf [4]byte
	if _, err := io.ReadFull(r, countBuf[:]); err != nil {
		return err
	}
	count := binary.LittleEndian.Uint32(countBuf[:])

	if count != uint32(len(g.nodes)) {
		return fmt.Errorf("neighbors node count mismatch: nodes section has %d, neighbors section has %d", len(g.nodes), count)
	}

	for i := uint32(0); i < count; i++ {
		node := &g.nodes[i]
		var lvlBuf [4]byte
		if _, err := io.ReadFull(r, lvlBuf[:]); err != nil {
			return err
		}
		level := int(binary.LittleEndian.Uint32(lvlBuf[:]))

		if !node.Alive {
			var numBuf [4]byte
			if _, err := io.ReadFull(r, numBuf[:]); err != nil {
				return err
			}
			node.NeighborsHandle = 0
			continue
		}

		buf, handle := g.allocator.Alloc(0, g.neighborsBlockSize(level)*8)
		for i := range buf {
			buf[i] = 0
		}
		node.NeighborsHandle = handle

		for lc := 0; lc <= level; lc++ {
			var numBuf [4]byte
			if _, err := io.ReadFull(r, numBuf[:]); err != nil {
				return err
			}
			numNeighbors := binary.LittleEndian.Uint32(numBuf[:])

			if numNeighbors > 0 {
				neighbors := make([]uint64, numNeighbors)
				sizeInBytes := numNeighbors * 8
				byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&neighbors[0])), sizeInBytes)
				if _, err := io.ReadFull(r, byteSlice); err != nil {
					return err
				}
				g.setNeighbors(handle, lc, neighbors)
			}
		}
	}

	return nil
}

func readSectionKeyMap(r io.Reader, vs *VectorStore) error {
	var countBuf [4]byte
	if _, err := io.ReadFull(r, countBuf[:]); err != nil {
		return err
	}
	count := binary.LittleEndian.Uint32(countBuf[:])

	for i := uint32(0); i < count; i++ {
		var idBuf [8]byte
		if _, err := io.ReadFull(r, idBuf[:]); err != nil {
			return err
		}
		id := binary.LittleEndian.Uint64(idBuf[:])

		var keyLenBuf [2]byte
		if _, err := io.ReadFull(r, keyLenBuf[:]); err != nil {
			return err
		}
		keyLen := binary.LittleEndian.Uint16(keyLenBuf[:])

		keyBytes := make([]byte, keyLen)
		if keyLen > 0 {
			if _, err := io.ReadFull(r, keyBytes); err != nil {
				return err
			}
		}
		key := string(keyBytes)

		vs.keys[id] = key
		vs.ids[key] = id
	}

	return nil
}
