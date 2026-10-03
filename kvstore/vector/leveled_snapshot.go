package vector

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"time"
	"unsafe"
)

// leveledMagic — магические байты заголовка бинарного снапшота LeveledVectorStore.
// Версия в байте [5]: v1 — без тенант-меты; v2 — после каждого frozen/SQ-сегмента
// сериализованы TenantCatalog + segmentAttrs (durability колоночного слоя); v3 —
// hnswSegment (flat, dim>256) тоже персистит атрибуты по вектору (writeAttrs),
// колонки+каталог регенерируются buildSegment на load. v1/v2 читаются как прежде.
// leveledMagic: "LVLV" + версия формата. v6 добавил 8B snapshotLSN-watermark
// в заголовок (после snapshotTime) — recovery-идемпотентность векторов. v7 —
// текстовый BM25-слой: frozen/SQ-сегменты сериализуют segmentText (writeSegText)
// после segMeta, hnswSegment персистит термы по вектору (writeTerms) после
// attrs — слой регенерируется buildSegment на load. Снапшоты v<7 читаются как
// прежде (текста в них нет → слой nil). v8 — документная секция после
// frozen-сегмента, v9 — та же секция у SQ8- и hnsw-сегментов. Сейчас секция
// пишется пустой, а читается ради снапшотов прежних версий (см.
// snapshot_docs_section.go).
var leveledMagic = [8]byte{'L', 'V', 'L', 'V', 0, 9, 0, 0}

// leveledMagicPrefix — общая часть ('LVLV') для проверки формата без версии.
var leveledMagicPrefix = [4]byte{'L', 'V', 'L', 'V'}

// =============================================================================
// Persistence: SaveBinary / LoadBinary
//
// Формат (little-endian):
//   [8B magic]
//   [8B snapshotTime int64 UnixNano]
//   [4B dim uint32]
//   [1B maxLevels uint8 — всегда 8]
//   для каждого уровня [0..maxLevels-1]:
//     [4B nSegs uint32]
//     для каждого сегмента:
//       [1B segType: 0=FROZEN, 1=HNSW_FLAT]
//       if FROZEN: fg.WriteTo(w)
//       if HNSW_FLAT:
//         [4B nVecs uint32]
//         для каждого вектора:
//           [2B keyLen uint16][keyLen B key][dim*4 B vec float32]
//
// Delta НЕ сохраняется (она пуста после FlushDeltaSync).
// Новые Add() после FlushDeltaSync попадают в WAL и восстанавливаются при старте.
// =============================================================================

// SaveBinary сохраняет все скомпакченные сегменты.
// ВАЖНО: вызывать после FlushDeltaSync() для гарантии полного снапшота.
func (lvs *LeveledVectorStore) SaveBinary(w io.Writer) error {
	// Снапшотируем указатели сегментов под RLock (дёшево — только указатели).
	// Сами данные сегментов иммутабельны (frozenSegment) или защищены своим мьютексом (hnswSegment).
	lvs.mu.RLock()
	dim := lvs.dim
	var snapLevels [maxLevels][]segment
	for i, level := range lvs.levels {
		if len(level) > 0 {
			snapLevels[i] = make([]segment, len(level))
			copy(snapLevels[i], level)
		}
	}
	// Снимаем tombstone-срез под тем же RLock — он консистентен с указателями
	// сегментов (множество удалённых ⊆ актуальному набору сегментов).
	tombSnap := lvs.tombstones.Load()
	lvs.mu.RUnlock()

	// CRC32 (v5+): весь payload пишется и в w, и в hasher; 4-байтный чексум
	// дописывается в КОНЕЦ (в out, не в hasher) и проверяется при загрузке.
	// Защита от тихого bit-rot в graph_leveled.bin (P0-3).
	out := w
	hasher := crc32.NewIEEE()
	w = io.MultiWriter(out, hasher)

	// Пишем без блокировки (frozenSegment иммутабелен, hnswSegment использует свой мьютекс)
	if _, err := w.Write(leveledMagic[:]); err != nil {
		return fmt.Errorf("leveled: write magic: %w", err)
	}

	var u8 [8]byte
	binary.LittleEndian.PutUint64(u8[:], uint64(time.Now().UnixNano()))
	if _, err := w.Write(u8[:]); err != nil {
		return fmt.Errorf("leveled: write snapshotTime: %w", err)
	}

	// LSN-watermark (v6): «снапшот покрывает векторные операции до этого LSN».
	// Значение выставлено SetSnapshotWatermark перед вызовом (под RLock снимаем).
	lvs.mu.RLock()
	wmLSN := lvs.snapshotLSN
	lvs.mu.RUnlock()
	binary.LittleEndian.PutUint64(u8[:], wmLSN)
	if _, err := w.Write(u8[:]); err != nil {
		return fmt.Errorf("leveled: write snapshotLSN: %w", err)
	}

	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(dim))
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("leveled: write dim: %w", err)
	}

	if _, err := w.Write([]byte{maxLevels}); err != nil {
		return fmt.Errorf("leveled: write nLevels: %w", err)
	}

	for lyr, level := range snapLevels {
		binary.LittleEndian.PutUint32(u4[:], uint32(len(level)))
		if _, err := w.Write(u4[:]); err != nil {
			return fmt.Errorf("leveled: write nSegs[%d]: %w", lyr, err)
		}
		for si, seg := range level {
			switch s := seg.(type) {
			case *frozenSegment:
				if _, err := w.Write([]byte{segTypeFrozen}); err != nil {
					return fmt.Errorf("leveled: write segType[%d][%d]: %w", lyr, si, err)
				}
				if err := s.fg.WriteGraphTo(w); err != nil {
					return fmt.Errorf("leveled: write frozen[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegMeta(w, s.cat, s.attrs); err != nil {
					return fmt.Errorf("leveled: write frozenMeta[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegText(w, s.text); err != nil {
					return fmt.Errorf("leveled: write frozenText[%d][%d]: %w", lyr, si, err)
				}
				if err := writeEmptyDocsSection(w); err != nil {
					return fmt.Errorf("leveled: write frozenDocs[%d][%d]: %w", lyr, si, err)
				}

			case *frozenSQSegment:
				if _, err := w.Write([]byte{segTypeFrozenSQ8}); err != nil {
					return fmt.Errorf("leveled: write segType[%d][%d]: %w", lyr, si, err)
				}
				if err := s.fg.WriteGraphToSQ(w); err != nil {
					return fmt.Errorf("leveled: write frozenSQ[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegMeta(w, s.cat, s.attrs); err != nil {
					return fmt.Errorf("leveled: write frozenSQMeta[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegText(w, s.text); err != nil {
					return fmt.Errorf("leveled: write frozenSQText[%d][%d]: %w", lyr, si, err)
				}
				if err := writeEmptyDocsSection(w); err != nil {
					return fmt.Errorf("leveled: write frozenSQDocs[%d][%d]: %w", lyr, si, err)
				}

			case *hnswSegment:
				if _, err := w.Write([]byte{segTypeHNSWFlat}); err != nil {
					return fmt.Errorf("leveled: write segType[%d][%d]: %w", lyr, si, err)
				}
				s.mu.RLock()
				// Термы по вектору (v7): node id = frozen id текстового слоя →
				// декодим постинги в пер-доковые списки один раз на сегмент.
				// На load buildSegment регенерирует segmentText из entries[].Terms
				// (как колонки из Attrs). decTerms==nil (текстов нет) → nTerms=0.
				decTerms := s.text.decodeTerms()
				// Считаем живые векторы
				nVecs := 0
				for id, key := range s.keys {
					if key != "" && s.g.nodes[id].Alive {
						nVecs++
					}
				}
				binary.LittleEndian.PutUint32(u4[:], uint32(nVecs))
				if _, err := w.Write(u4[:]); err != nil {
					s.mu.RUnlock()
					return fmt.Errorf("leveled: write nVecs[%d][%d]: %w", lyr, si, err)
				}

				var klen [2]byte
				var writeErr error
				for id, key := range s.keys {
					if key == "" || !s.g.nodes[id].Alive {
						continue
					}
					var entryTerms []TermTF
					if int(id) < len(decTerms) {
						entryTerms = decTerms[id]
					}
					vec := s.g.arena.Get(s.g.nodes[id].VectorOffset)

					binary.LittleEndian.PutUint16(klen[:], uint16(len(key)))
					if _, err := w.Write(klen[:]); err != nil {
						writeErr = err
						break
					}
					if _, err := io.WriteString(w, key); err != nil {
						writeErr = err
						break
					}
					if dim > 0 && len(vec) > 0 {
						b := unsafe.Slice((*byte)(unsafe.Pointer(&vec[0])), len(vec)*4)
						if _, err := w.Write(b); err != nil {
							writeErr = err
							break
						}
					}
					// Атрибуты вектора (v3): node id = индекс колонки → decodeAt(id).
					// hnsw-сегмент на load перестраивается buildSegment(entries), который
					// регенерирует колонки+каталог из entries[].Attrs (см. readAttrs ниже).
					if err := writeAttrs(w, s.attrs.decodeAt(int(id))); err != nil {
						writeErr = err
						break
					}
					if err := writeTerms(w, entryTerms); err != nil {
						writeErr = err
						break
					}
				}
				if writeErr == nil {
					writeErr = writeEmptyDocsSection(w)
				}
				s.mu.RUnlock()
				if writeErr != nil {
					return fmt.Errorf("leveled: write hnswFlat[%d][%d]: %w", lyr, si, writeErr)
				}
			}
		}
	}

	// Tombstones (v4+): ключи, удалённые из иммутабельных сегментов, но ещё не
	// вырезанные физически (это делает merge). Без их персиста Delete не переживает
	// рестарт — вектор остаётся в данных сегмента и «воскресает» (см. P0-1).
	var tcount uint32
	if tombSnap != nil {
		tcount = uint32(len(*tombSnap))
	}
	binary.LittleEndian.PutUint32(u4[:], tcount)
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("leveled: write nTombstones: %w", err)
	}
	if tombSnap != nil {
		var tklen [2]byte
		for key := range *tombSnap {
			binary.LittleEndian.PutUint16(tklen[:], uint16(len(key)))
			if _, err := w.Write(tklen[:]); err != nil {
				return fmt.Errorf("leveled: write tombstoneKeyLen: %w", err)
			}
			if _, err := io.WriteString(w, key); err != nil {
				return fmt.Errorf("leveled: write tombstoneKey: %w", err)
			}
		}
	}

	// CRC32-трейлер (v5+): чексум всего payload выше. Пишется в out (НЕ в hasher).
	binary.LittleEndian.PutUint32(u4[:], hasher.Sum32())
	if _, err := out.Write(u4[:]); err != nil {
		return fmt.Errorf("leveled: write crc: %w", err)
	}

	return nil
}

// LoadBinary восстанавливает состояние из ридера.
// После загрузки delta пуста — новые Add() идут туда, WAL replay добавит недостающие.
//
// Всё-или-ничего: payload целиком читается в локальные переменные и сверяется
// CRC, и только потом коммитится в lvs. При любой ошибке (bit-rot, усечённый
// файл) стор остаётся ровно в прежнем состоянии — иначе recovery реплеил бы WAL
// поверх наполовину загруженных сегментов (тихо-неконсистентная база).
func (lvs *LeveledVectorStore) LoadBinary(r io.Reader) error {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// CRC32 (v5+): весь payload читаем через tee → hasher; 4-байтный трейлер
	// читаем отдельно из crcSource (тот же буферизованный поток, но мимо tee)
	// и сверяем. Все ReadFull ниже идут через r.
	//
	// Источник буферизуется здесь: формат читается полями по 2–4 байта, и на
	// голом *os.File каждое поле было отдельным системным вызовом — снапшот
	// 18 МБ с текстовым слоем грузился 2,2–2,8 с против 0,13–0,23 с через
	// буфер. Буфер читает вперёд, поэтому r не должен содержать ничего после
	// снапшота.
	size, sized := sourceSize(r)
	buffered := bufio.NewReaderSize(r, 256<<10)
	crcSource := buffered
	hasher := crc32.NewIEEE()
	r = io.TeeReader(buffered, hasher)
	// Бюджет: счётчики из файла сверяются с его остатком до выделения памяти
	// (snapshot_budget.go) — CRC проверяется только в конце.
	if sized {
		r = &budgetReader{r: r, left: size}
	}

	// Проверяем магию: префикс 'LVLV' обязателен, версия в байте [5].
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("leveled: read magic: %w", err)
	}
	if [4]byte(magic[:4]) != leveledMagicPrefix {
		return fmt.Errorf("leveled: invalid magic header (not a graph_leveled.bin?)")
	}
	version := magic[5]
	if version < 1 || version > 9 {
		return fmt.Errorf("leveled: unsupported snapshot version %d", version)
	}

	// ── Фаза чтения: только локальные переменные, lvs не трогаем до коммита. ──

	// Читаем snapshotTime
	var u8 [8]byte
	if _, err := io.ReadFull(r, u8[:]); err != nil {
		return fmt.Errorf("leveled: read snapshotTime: %w", err)
	}
	snapTime := int64(binary.LittleEndian.Uint64(u8[:]))

	// LSN-watermark (v6+): recovery пропустит векторные WAL-записи с LSN ≤ этого.
	// Старые снапшоты (v≤5) без поля → watermark 0 (реплей всех записей, как раньше;
	// дедуп #1 удержит корректность).
	var snapLSN uint64
	if version >= 6 {
		if _, err := io.ReadFull(r, u8[:]); err != nil {
			return fmt.Errorf("leveled: read snapshotLSN: %w", err)
		}
		snapLSN = binary.LittleEndian.Uint64(u8[:])
	}

	// Читаем dim
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return fmt.Errorf("leveled: read dim: %w", err)
	}
	dim := int(binary.LittleEndian.Uint32(u4[:]))

	// Читаем nLevels
	var nLevelsBuf [1]byte
	if _, err := io.ReadFull(r, nLevelsBuf[:]); err != nil {
		return fmt.Errorf("leveled: read nLevels: %w", err)
	}
	nLevels := int(nLevelsBuf[0])
	if nLevels > maxLevels {
		return fmt.Errorf("leveled: nLevels=%d превышает maxLevels=%d (повреждён заголовок?)", nLevels, maxLevels)
	}

	// stagedSeg — сегмент, прочитанный, но ещё не закоммиченный в lvs.levels.
	// frozen/frozenSQ читаются сразу (обычная Go-память — при abort подберёт GC);
	// hnsw откладывается сырыми entries: сборка графа дорогая, строим его только
	// после успешного CRC (заодно не жжём CPU на битом файле).
	type stagedSeg struct {
		seg     segment
		entries []DeltaEntry
	}
	var staged [maxLevels][]stagedSeg

	// Восстанавливаем каждый уровень
	for lyr := 0; lyr < nLevels; lyr++ {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return fmt.Errorf("leveled: read nSegs[%d]: %w", lyr, err)
		}
		nSegs := int(binary.LittleEndian.Uint32(u4[:]))
		if nSegs == 0 {
			continue
		}
		if err := needBytes(r, int64(nSegs), 1); err != nil {
			return fmt.Errorf("leveled: segments[%d]: %w", lyr, err)
		}

		for si := 0; si < nSegs; si++ {
			var stBuf [1]byte
			if _, err := io.ReadFull(r, stBuf[:]); err != nil {
				return fmt.Errorf("leveled: read segType[%d][%d]: %w", lyr, si, err)
			}
			segType := stBuf[0]

			switch segType {
			case segTypeFrozen:
				fg, err := ReadFrozenGraph(r, lvs.cfg.Distance)
				if err != nil {
					return fmt.Errorf("leveled: read frozen[%d][%d]: %w", lyr, si, err)
				}
				seg := &frozenSegment{fg: fg}
				if version >= 2 {
					if seg.cat, seg.attrs, err = readSegMeta(r); err != nil {
						return fmt.Errorf("leveled: read frozenMeta[%d][%d]: %w", lyr, si, err)
					}
				}
				if version >= 7 {
					if seg.text, err = readSegText(r); err != nil {
						return fmt.Errorf("leveled: read frozenText[%d][%d]: %w", lyr, si, err)
					}
				}
				if version >= 8 {
					docs, err := readDocsSection(r, fg.n)
					if err != nil {
						return fmt.Errorf("leveled: read frozenDocs[%d][%d]: %w", lyr, si, err)
					}
					if hasAnyDoc(docs) {
						all := mergeDocsIntoEntries(frozenEntries(seg), docs)
						restoreDocVectors(fg, docs)
						seg.attrs = buildSegmentAttrs(all, nil)
						seg.text = buildSegmentText(all)
					}
				}
				staged[lyr] = append(staged[lyr], stagedSeg{seg: seg})

			case segTypeFrozenSQ8:
				fg, err := ReadFrozenGraphSQ(r, lvs.cfg.Metric)
				if err != nil {
					return fmt.Errorf("leveled: read frozenSQ[%d][%d]: %w", lyr, si, err)
				}
				seg := &frozenSQSegment{fg: fg}
				if version >= 2 {
					if seg.cat, seg.attrs, err = readSegMeta(r); err != nil {
						return fmt.Errorf("leveled: read frozenSQMeta[%d][%d]: %w", lyr, si, err)
					}
				}
				if version >= 7 {
					if seg.text, err = readSegText(r); err != nil {
						return fmt.Errorf("leveled: read frozenSQText[%d][%d]: %w", lyr, si, err)
					}
				}
				// У SQ8 секция появилась в v9; в снапшотах v8 её нет.
				if version >= 9 {
					docs, err := readDocsSection(r, fg.n)
					if err != nil {
						return fmt.Errorf("leveled: read frozenSQDocs[%d][%d]: %w", lyr, si, err)
					}
					if hasAnyDoc(docs) {
						all := mergeDocsIntoEntries(frozenSQEntries(seg), docs)
						restoreDocVectorsSQ(fg, docs)
						seg.attrs = buildSegmentAttrs(all, nil)
						seg.text = buildSegmentText(all)
					}
				}
				staged[lyr] = append(staged[lyr], stagedSeg{seg: seg})

			case segTypeHNSWFlat:
				if _, err := io.ReadFull(r, u4[:]); err != nil {
					return fmt.Errorf("leveled: read nVecs[%d][%d]: %w", lyr, si, err)
				}
				nVecs := int(binary.LittleEndian.Uint32(u4[:]))
				// Вектор на диске: длина ключа и dim float32.
				if err := needBytes(r, int64(nVecs), 2+int64(dim)*4); err != nil {
					return fmt.Errorf("leveled: hnsw[%d][%d]: %w", lyr, si, err)
				}

				entries := make([]DeltaEntry, 0, nVecs)
				var klen [2]byte
				for vi := 0; vi < nVecs; vi++ {
					if _, err := io.ReadFull(r, klen[:]); err != nil {
						return fmt.Errorf("leveled: read keyLen[%d][%d][%d]: %w", lyr, si, vi, err)
					}
					kl := int(binary.LittleEndian.Uint16(klen[:]))
					key := ""
					if kl > 0 {
						kb := make([]byte, kl)
						if _, err := io.ReadFull(r, kb); err != nil {
							return fmt.Errorf("leveled: read key[%d][%d][%d]: %w", lyr, si, vi, err)
						}
						key = string(kb)
					}
					vec := make([]float32, dim)
					if dim > 0 {
						vb := unsafe.Slice((*byte)(unsafe.Pointer(&vec[0])), dim*4)
						if _, err := io.ReadFull(r, vb); err != nil {
							return fmt.Errorf("leveled: read vec[%d][%d][%d]: %w", lyr, si, vi, err)
						}
					}
					var attrs Attributes
					if version >= 3 {
						a, err := readAttrs(r)
						if err != nil {
							return fmt.Errorf("leveled: read hnswAttrs[%d][%d][%d]: %w", lyr, si, vi, err)
						}
						attrs = a
					}
					var terms []TermTF
					if version >= 7 {
						t, err := readTerms(r)
						if err != nil {
							return fmt.Errorf("leveled: read hnswTerms[%d][%d][%d]: %w", lyr, si, vi, err)
						}
						terms = t
					}
					entries = append(entries, DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms})
				}

				// Документы секции (снапшоты прежних версий) лежат на тех же
				// позициях, где в цикле выше прочитаны нули и пустые атрибуты.
				if version >= 9 {
					docs, err := readDocsSection(r, len(entries))
					if err != nil {
						return fmt.Errorf("leveled: read hnswDocs[%d][%d]: %w", lyr, si, err)
					}
					if hasAnyDoc(docs) {
						entries = mergeDocsIntoEntries(entries, docs)
					}
				}

				if len(entries) > 0 {
					staged[lyr] = append(staged[lyr], stagedSeg{entries: entries})
				}

			default:
				return fmt.Errorf("leveled: unknown segType=%d at [%d][%d]", segType, lyr, si)
			}
		}
	}

	// Tombstones (v4+): восстанавливаем множество удалённых-но-не-вырезанных ключей.
	// Без этого удаления, сделанные до снапшота, «воскресают» после рестарта (P0-1).
	// Старые снапшоты (v<4) секции не имеют → мапа остаётся пустой (прежнее поведение).
	var tomb map[string]struct{}
	if version >= 4 {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return fmt.Errorf("leveled: read nTombstones: %w", err)
		}
		tcount := int(binary.LittleEndian.Uint32(u4[:]))
		if err := needBytes(r, int64(tcount), 2); err != nil {
			return fmt.Errorf("leveled: tombstones: %w", err)
		}
		if tcount > 0 {
			tomb = make(map[string]struct{}, tcount)
			var tklen [2]byte
			for i := 0; i < tcount; i++ {
				if _, err := io.ReadFull(r, tklen[:]); err != nil {
					return fmt.Errorf("leveled: read tombstoneKeyLen[%d]: %w", i, err)
				}
				kl := int(binary.LittleEndian.Uint16(tklen[:]))
				kb := make([]byte, kl)
				if _, err := io.ReadFull(r, kb); err != nil {
					return fmt.Errorf("leveled: read tombstoneKey[%d]: %w", i, err)
				}
				tomb[string(kb)] = struct{}{}
			}
		}
	}

	// CRC32-трейлер (v5+): сверяем чексум всего прочитанного payload. Ловит тихий
	// bit-rot graph_leveled.bin до того, как битые данные попадут в индекс (P0-3).
	if version >= 5 {
		var crcBuf [4]byte
		if _, err := io.ReadFull(crcSource, crcBuf[:]); err != nil {
			return fmt.Errorf("leveled: read crc: %w", err)
		}
		want := binary.LittleEndian.Uint32(crcBuf[:])
		if got := hasher.Sum32(); got != want {
			return fmt.Errorf("leveled: snapshot CRC mismatch: got %08x want %08x (повреждён graph_leveled.bin)", got, want)
		}
	}

	// ── Фаза коммита: payload целиком валиден, ошибок ниже нет. ──

	lvs.snapshotTime = snapTime
	lvs.snapshotLSN = snapLSN
	lvs.dim = dim

	for i := range lvs.levels {
		lvs.levels[i] = nil
	}
	for lyr := range staged {
		if len(staged[lyr]) == 0 {
			continue
		}
		lvs.levels[lyr] = make([]segment, 0, len(staged[lyr]))
		for _, ss := range staged[lyr] {
			seg := ss.seg
			if seg == nil {
				seg = lvs.buildSegment(ss.entries, dim)
			}
			if seg != nil {
				lvs.levels[lyr] = append(lvs.levels[lyr], seg)
			}
		}
	}

	if tomb != nil {
		lvs.tombstones.Store(&tomb)
	}

	// Инициализируем пустую дельту (новые Add() пойдут сюда)
	if lvs.delta != nil {
		lvs.delta.Close() // освободить аллокатор старой дельты
	}
	if dim > 0 {
		max := deltaMax(dim, lvs.cfg.DeltaMax)
		lvs.delta = NewDeltaSegmentSharded(dim, max, lvs.cfg.Distance, lvs.cfg.M, lvs.cfg.EfConstruction, lvs.deltaShardCount())
	}

	lvs.touchMutation()
	return nil
}
