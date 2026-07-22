package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Типы операций
const (
	OpSet     byte = 1
	OpDel     byte = 2
	OpExpire  byte = 3 // TTL: Value = 8 байт unix nano (абсолютное время смерти)
	OpPersist byte = 4 // Убрать TTL: Value пустой
	OpVSimAdd byte = 5 // вектор добавлен в HNSW
	OpVSimDel byte = 6 // вектор удалён из HNSW
	// OpVSimAddAttrs: вектор + атрибуты (колоночный слой). Value кодируется
	// vector.SerializeVectorWithAttrs. Replay восстанавливает attrs/tenant через
	// AddWithAttrs (P0-4). Эмитится write-путём, когда AddWithAttrs выходит в RESP.
	OpVSimAddAttrs byte = 7
	// OpVSimAddDoc: вектор + атрибуты + термы текста (BM25). Value кодируется
	// vector.SerializeVectorWithDoc — в WAL едут ТЕРМЫ, не сырой текст (реплей
	// не перетокенизирует: журнал самодостаточен, состояние воспроизводится
	// бит-в-бит независимо от версии стеммера). Реплей через AddDocTerms.
	// Эмитится write-путём, когда VSIM.ADDDOC выходит в RESP (шаг 7 спринта).
	OpVSimAddDoc byte = 8
	// OpVSimAddDocBatch: НЕСКОЛЬКО доков одной атомарной записью (один CRC на
	// всю запись → торн-хвост после краша отбрасывает батч целиком, частичное
	// применение невоспроизводимо реплеем ПО ПОСТРОЕНИЮ). Появился для пары
	// supersedes VMEM.REMEMBER (шаг 7 VMEM_DESIGN): закрытая цель + наследник
	// порознь дали бы после краша либо «два истинных сейчас», либо «закрыт без
	// наследника». Value кодируется vector.SerializeDocBatch (Key записи =
	// ключ первого дока, информативно). Реплей: AddDocTerms по порядку.
	OpVSimAddDocBatch byte = 9
	OpZAdd            byte = 16 // sorted set: Key = setName, Value = [8B score LE float64][member string]
	OpZRem            byte = 17 // sorted set: Key = setName, Value = [member string]
)

// maxEntrySize — защита от мусорных данных при recovery.
// Если length > 64MB — это скорее всего corruption, а не настоящая запись.
const maxEntrySize = 64 * 1024 * 1024

// Entry — одна запись в WAL.
//
// LSN — монотонный номер записи (Log Sequence Number), присваивается писателем
// при кодировании и входит в payload под CRC. Курсор для будущей резюмируемой
// репликации (реплика стримит от последнего виденного LSN вместо full resync).
// В snapshot-записях LSN=0 (watermark хранится в заголовке файла, не в записи).
type Entry struct {
	LSN   uint64
	Op    byte
	Key   string
	Value []byte
}

// WAL — Write-Ahead Log с поддержкой ротации.
//
// Durability trade-off (осознанный выбор, аналог Redis AOF everysec):
//
// Текущая архитектура — fire-and-forget: BatchWAL.Write() отправляет
// запись в канал и немедленно возвращает управление. Данные попадают на
// диск асинхронно (через flusher + Syncer fsync каждые 100ms).
//
// Это означает окно потери данных ≤100ms при crash. Для in-memory
// KV store это приемлемый компромисс: максимальный throughput (~1.2M ops/sec)
// ценой теоретической потери последних ~100ms данных.
//
// Для 100% durability в будущем планируется group commit (аналог PostgreSQL):
// flusher будет делать fsync после каждого batch и уведомлять writers.
// Ожидаемая деградация throughput: ~10-15% на NVMe.
type WAL struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	dir    string // директория, где лежат WAL-файлы

	// nextLSN — следующий свободный номер записи. Присваивает ЕДИНСТВЕННЫЙ
	// писатель (batch-flusher; evictor тоже идёт через тот же канал), поэтому
	// порядок LSN == порядок в файле держится как инвариант. atomic оставляет
	// путь race-free даже если WAL.Write и flusher случайно смешаются в тестах.
	// Стартует с 1; 0 зарезервирован под «нет LSN» (snapshot-записи).
	// recovery ставит SetNextLSN(maxLSN+1) до приёма трафика.
	nextLSN atomic.Uint64

	// failErr латчит ПЕРВУЮ фатальную ошибку персистентности (ENOSPC, I/O error
	// при write/flush/fsync). Однажды взведён — не снимается в рамках процесса:
	// после первого провалившегося сброса in-memory состояние и WAL на диске уже
	// разошлись (батч, подтверждённый клиенту через fire-and-forget, потерян),
	// поэтому продолжать принимать записи = множить тихую потерю. Снятие только
	// через рестарт (recovery из snapshot + чистый WAL). Хранит *error, чтобы
	// Failed() отдавал первопричину оператору.
	//
	// Промышленный аналог: Redis stop-writes-on-bgsave-error (по умолчанию
	// отклоняет записи при ошибке персистентности), Postgres — PANIC на ошибке
	// записи WAL. Тихо подтверждать потерянные записи — durability-грех №1.
	failErr atomic.Pointer[error]
}

// fail латчит первую фатальную ошибку записи. Повторные вызовы игнорируются
// (CompareAndSwap), чтобы Failed() отдавал первопричину, а не последнее эхо.
func (w *WAL) fail(err error) {
	if err == nil {
		return
	}
	e := err
	w.failErr.CompareAndSwap(nil, &e)
}

// Failed возвращает залатченную фатальную ошибку персистентности (nil если WAL
// здоров). Write-путь сервера ОБЯЗАН отклонять мутации, когда Failed()!=nil —
// иначе клиент получает OK на запись, которая уже не попадёт на диск.
func (w *WAL) Failed() error {
	if p := w.failErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Open открывает или создаёт WAL-файл.
//
// Для свежесозданного (пустого) файла пишет заголовок [MAGIC][VER][baseLSN=0].
// При переоткрытии непустого файла заголовок уже на месте — не трогаем.
func Open(path string) (*WAL, error) {
	dir := filepath.Dir(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal open: %w", err)
	}

	w := &WAL{
		file:   file,
		writer: bufio.NewWriter(file),
		dir:    dir,
	}
	// nextLSN стартует с 1; recovery перезапишет через SetNextLSN до трафика.
	w.nextLSN.Store(1)

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("wal stat: %w", err)
	}
	if info.Size() == 0 {
		if err := writeFileHeader(w.writer, walBaseLSN); err != nil {
			file.Close()
			return nil, err
		}
	}
	return w, nil
}

// FsyncDir делает durable последний rename/create/unlink внутри директории,
// синхронизируя саму директорию. Без этого потеря питания может откатить
// переименование (данные файла уже на диске, но запись в каталоге — нет):
// snapshot.wal/graph_leveled.bin «исчезнут», хотя старые WAL уже удалены → потеря.
// Так же поступают SQLite/LMDB/PostgreSQL после атомарной замены файла.
func FsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("fsync dir open %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}

// reserveLSN резервирует n подряд идущих номеров и возвращает первый из них.
// Присваивание start..start+n-1 монотонно. Один atomic Add на пачку.
func (w *WAL) reserveLSN(n uint64) uint64 {
	return w.nextLSN.Add(n) - n
}

// SetNextLSN выставляет счётчик перед приёмом трафика (recovery).
func (w *WAL) SetNextLSN(v uint64) {
	w.nextLSN.Store(v)
}

// LastLSN возвращает последний присвоенный номер (nextLSN-1), 0 если записей не
// было. Используется как watermark при компакции.
func (w *WAL) LastLSN() uint64 {
	v := w.nextLSN.Load()
	if v == 0 {
		return 0
	}
	return v - 1
}

// Write записывает одну операцию в WAL.
// Формат записи: [CRC32 4B][PayloadLen 4B][LSN 8B][Op 1B][KeyLen 4B][Key][Value]
// LSN входит в payload → накрыт CRC.
func (w *WAL) Write(entry Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Резерв LSN под тем же локом, что и запись → порядок LSN == порядок в файле.
	lsn := w.reserveLSN(1)
	payload := encodeEntry(lsn, entry)
	checksum := crc32.ChecksumIEEE(payload)

	// Создаем локальный массив на 8 байт (4 байта для CRC32 + 4 байта для длины).
	// Важно: так как размер фиксирован, Go выделит эту память на стеке,
	// а не в куче. Это значит — НОЛЬ нагрузки на сборщик мусора (GC)!
	var header [8]byte

	// Вручную раскладываем числа по байтам (без рефлексии, работает мгновенно)
	binary.LittleEndian.PutUint32(header[0:4], checksum)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(payload)))

	// Пишем весь заголовок (8 байт) за один вызов
	if _, err := w.writer.Write(header[:]); err != nil {
		werr := fmt.Errorf("wal write header: %w", err)
		w.fail(werr)
		return werr
	}

	// Пишем сами данные
	if _, err := w.writer.Write(payload); err != nil {
		werr := fmt.Errorf("wal write payload: %w", err)
		w.fail(werr)
		return werr
	}

	return nil
}

// Sync сбрасывает буфер на диск (fsync).
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writer.Flush(); err != nil {
		w.fail(err)
		return err
	}
	if err := w.file.Sync(); err != nil {
		w.fail(err)
		return err
	}
	return nil
}

// Rotate переключает WAL на новый файл.
// Старый файл закрывается и возвращается его путь.
// Эта операция МГНОВЕННАЯ — блокировка на наносекунды.
//
// Схема:
//  1. Создаём новый файл (wal_0002.log)
//  2. Lock → переключаем writer → Unlock
//  3. Возвращаем путь к старому файлу (wal_0001.log)
func (w *WAL) Rotate(newPath string) (oldPath string, err error) {
	// Создаём новый файл ДО блокировки — тяжёлая FS-операция вне лока
	newFile, err := os.OpenFile(newPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return "", fmt.Errorf("wal rotate: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Сбрасываем буфер старого WAL на диск
	if err := w.writer.Flush(); err != nil {
		newFile.Close()
		os.Remove(newPath)
		return "", fmt.Errorf("wal rotate flush: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		newFile.Close()
		os.Remove(newPath)
		return "", fmt.Errorf("wal rotate sync: %w", err)
	}

	oldPath = w.file.Name()
	if err := w.file.Close(); err != nil {
		// Данные уже synced — логируем, но продолжаем ротацию.
		slog.Warn("WAL rotate: error closing old file", "file", oldPath, "err", err)
	}

	w.file = newFile
	w.writer = bufio.NewWriter(newFile)

	// Новый файл всегда пуст → пишем заголовок (baseLSN=0 для WAL).
	// nextLSN НЕ сбрасываем — номера продолжаются сквозь ротацию.
	if err := writeFileHeader(w.writer, walBaseLSN); err != nil {
		return "", fmt.Errorf("wal rotate header: %w", err)
	}

	// fsync каталога: делаем durable появление нового WAL-файла. Без этого
	// power-loss может потерять запись каталога о новом файле (его данные
	// синкаются позже Syncer'ом, но имя в каталоге ещё не durable).
	if err := FsyncDir(w.dir); err != nil {
		return "", fmt.Errorf("wal rotate dir fsync: %w", err)
	}

	return oldPath, nil
}

// Close закрывает WAL.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writer.Flush(); err != nil {
		w.file.Close()
		return fmt.Errorf("wal flush on close: %w", err)
	}
	// fsync на закрытии: Flush только сбрасывает bufio в page cache ОС.
	// Без Sync потеря питания сразу после штатной остановки теряет последний
	// батч (уже подтверждённый клиенту). Close — холодный путь (shutdown),
	// поэтому стоимость fsync здесь приемлема.
	if err := w.file.Sync(); err != nil {
		w.file.Close()
		return fmt.Errorf("wal fsync on close: %w", err)
	}
	return w.file.Close()
}

// Path возвращает текущий путь WAL-файла.
func (w *WAL) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Name()
}

// --- Кодирование/декодирование ---

func encodeEntry(lsn uint64, e Entry) []byte {
	size := 8 + 1 + 4 + len(e.Key) + len(e.Value)
	buf := make([]byte, size)

	offset := 0
	binary.LittleEndian.PutUint64(buf[offset:], lsn)
	offset += 8

	buf[offset] = e.Op
	offset++

	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(e.Key)))
	offset += 4

	// copy из string напрямую — без промежуточного []byte(e.Key).
	copy(buf[offset:], e.Key)
	offset += len(e.Key)

	if len(e.Value) > 0 {
		copy(buf[offset:], e.Value)
	}

	return buf
}

// ReadEntries читает все записи из одного WAL-файла.
//
// Стратегия recovery (аналог PostgreSQL):
//   - Читаем записи последовательно до первой невалидной
//   - Truncated header/payload → нормально при crash recovery, логируем
//   - CRC mismatch → corruption, логируем и останавливаемся
//   - Реальная I/O ошибка → возвращаем error
//   - Всё что прочитано до ошибки — валидные записи
func ReadEntries(path string) ([]Entry, error) {
	_, entries, err := ReadFile(path)
	return entries, err
}

// ReadFile читает заголовок + все записи одного файла (WAL или snapshot).
// Возвращает baseLSN из заголовка (для snapshot — watermark, для WAL — 0) и
// записи с заполненным полем LSN. Отсутствующий/пустой файл → (0, nil, nil).
//
// Незнакомая магия или версия → ошибка (dual-format нет: смена формата до
// прода = чистый старт).
func ReadFile(path string) (uint64, []Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("wal read: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	baseName := filepath.Base(path)

	// Заголовок: пустой файл (io.EOF) → нет записей, не ошибка.
	baseLSN, err := readFileHeader(reader)
	if err == io.EOF {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("wal read %s: %w", baseName, err)
	}

	var entries []Entry

	for {
		// 1. Читаем header: [CRC32 4B][Length 4B]
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if err != nil {
			if err == io.EOF {
				break // Нормальный конец файла
			}
			if err == io.ErrUnexpectedEOF {
				// Truncated header — ожидаемо после crash
				slog.Warn("WAL: truncated header (crash recovery)",
					"wal", baseName, "entry", len(entries), "recovered", len(entries))
				break
			}
			// Реальная I/O ошибка — возвращаем что есть + error
			return baseLSN, entries, fmt.Errorf("wal read header %s: %w", baseName, err)
		}

		checksum := binary.LittleEndian.Uint32(header[0:4])
		length := binary.LittleEndian.Uint32(header[4:8])

		// Защита от мусорных данных: если length > maxEntrySize — это corruption
		if length > maxEntrySize {
			slog.Warn("WAL: suspicious entry length, stopping recovery",
				"wal", baseName, "length", length, "entry", len(entries), "recovered", len(entries))
			break
		}

		// 2. Читаем payload
		payload := make([]byte, length)
		_, err = io.ReadFull(reader, payload)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				slog.Warn("WAL: truncated payload (crash recovery)",
					"wal", baseName, "entry", len(entries), "recovered", len(entries))
				break
			}
			return baseLSN, entries, fmt.Errorf("wal read payload %s: %w", baseName, err)
		}

		// 3. CRC проверка
		if crc32.ChecksumIEEE(payload) != checksum {
			slog.Warn("WAL: CRC mismatch, stopping recovery",
				"wal", baseName, "entry", len(entries), "recovered", len(entries))
			break
		}

		// 4. Декодируем
		entry, err := decodeEntry(payload)
		if err != nil {
			slog.Warn("WAL: decode error, stopping recovery",
				"wal", baseName, "entry", len(entries), "err", err, "recovered", len(entries))
			break
		}
		entries = append(entries, entry)
	}

	return baseLSN, entries, nil
}

func decodeEntry(data []byte) (Entry, error) {
	// Минимум: LSN(8) + Op(1) + KeyLen(4) = 13 байт.
	if len(data) < 13 {
		return Entry{}, fmt.Errorf("entry too short: %d bytes", len(data))
	}

	offset := 0
	lsn := binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	op := data[offset]
	offset++

	keyLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4

	if offset+int(keyLen) > len(data) {
		return Entry{}, fmt.Errorf("invalid key length")
	}
	key := string(data[offset : offset+int(keyLen)])
	offset += int(keyLen)

	var value []byte
	if offset < len(data) {
		value = make([]byte, len(data)-offset)
		copy(value, data[offset:])
	}

	return Entry{LSN: lsn, Op: op, Key: key, Value: value}, nil
}

// ReadAllWALs читает snapshot + все WAL-файлы в правильном порядке.
// Порядок: snapshot.wal (если есть) → wal_0001.log → wal_0002.log → ...
func ReadAllWALs(dir string) ([]Entry, error) {
	var allEntries []Entry

	// 1. Сначала snapshot
	snapshotPath := filepath.Join(dir, "snapshot.wal")
	if entries, err := ReadEntries(snapshotPath); err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	} else if entries != nil {
		allEntries = append(allEntries, entries...)
	}

	// 2. Потом все WAL-файлы по порядку
	matches, _ := filepath.Glob(filepath.Join(dir, "wal_*.log"))
	sort.Strings(matches) // wal_0001.log, wal_0002.log, ...

	for _, path := range matches {
		entries, err := ReadEntries(path)
		if err != nil {
			return nil, fmt.Errorf("read wal %s: %w", path, err)
		}
		allEntries = append(allEntries, entries...)
	}

	return allEntries, nil
}

// CleanupOldWALs удаляет WAL-файлы, которые старше snapshot.
// Вызывается после успешного создания snapshot.
func CleanupOldWALs(dir string, keepPath string) error {
	matches, _ := filepath.Glob(filepath.Join(dir, "wal_*.log"))
	for _, path := range matches {
		if path == keepPath {
			continue // не удаляем текущий WAL
		}
		// Удаляем только если имя файла "меньше" текущего (более старый)
		if strings.Compare(filepath.Base(path), filepath.Base(keepPath)) < 0 {
			os.Remove(path)
		}
	}
	return nil
}

// WriteBatch записывает pre-encoded batch за одну блокировку.
//
// buf уже содержит все entries в формате [CRC32][Len][Payload]...
// Вызывается из BatchWAL.flushBatch — один раз на batch.
//
// Стоимость: 1 mutex lock + 1 bufio write.
// Сравни с Write(): 1 mutex lock + encode + CRC + 2 writes PER ENTRY.
func (w *WAL) WriteBatch(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}

	w.mu.Lock()
	_, err := w.writer.Write(buf)
	w.mu.Unlock()

	if err != nil {
		werr := fmt.Errorf("wal write batch: %w", err)
		w.fail(werr)
		return werr
	}
	return nil
}
