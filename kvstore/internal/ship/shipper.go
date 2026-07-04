package ship

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxChunkSize ограничивает размер одного объекта-куска (и пиковую память
	// шиппера). Большой backlog уезжает несколькими кусками за один тик.
	maxChunkSize = 16 * 1024 * 1024

	snapshotFile = "snapshot.wal"
	graphFile    = "graph_leveled.bin"
)

// Options — настройки шиппера.
type Options struct {
	// Interval — период тика. RPO при аварии ≈ Interval + время загрузки.
	Interval time.Duration
	// RetainManifests — сколько последних манифестов (restore-точек) хранить.
	// Объекты, на которые не ссылается ни один удерживаемый манифест,
	// удаляются ретеншном. Минимум 1.
	RetainManifests int
}

// Shipper — фоновый доставщик durability-набора на Remote.
//
// Работает только с файлами каталога данных (append-only WAL + иммутабельные
// снапшоты), не трогая движок: ни локов, ни каналов, ни задержки write-пути.
// Ошибки доставки НЕ блокируют запись — это осознанный async-контракт
// (RPO = лаг доставки), в отличие от локального durability fail-stop.
type Shipper struct {
	remote  Remote
	dataDir string
	opts    Options

	stop chan struct{}
	done chan struct{}

	// Состояние доставки. Мутируется только горутиной run() (single-writer).
	gen      uint64
	wals     map[string]*WALFile // имя файла → загруженные куски
	snapshot *FileRef
	graph    *FileRef
	// published отражает, что текущее состояние опубликовано манифестом.
	published bool

	// lastOKUnixNano — время последней успешной публикации (для gauge).
	lastOKUnixNano atomic.Int64
	// lagBytes — оценка недошипленных байт на конец последнего тика (gauge).
	lagBytes atomic.Int64
	// errState сглаживает лог: пишем на смене состояния (ошибка↔успех), а не
	// каждым тиком — иначе недоступный remote зальёт лог с частотой Interval.
	errState atomic.Bool

	initOnce sync.Once
}

// New создаёт шиппер. Start запускает фоновый цикл; для детерминированных
// тестов можно звать Tick напрямую без Start.
func New(remote Remote, dataDir string, opts Options) *Shipper {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	if opts.RetainManifests < 1 {
		opts.RetainManifests = 1
	}
	return &Shipper{
		remote:  remote,
		dataDir: dataDir,
		opts:    opts,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		wals:    map[string]*WALFile{},
	}
}

// LastSuccessUnixNano — unix-наносекунды последней успешной публикации (0 = ещё не было).
func (s *Shipper) LastSuccessUnixNano() int64 { return s.lastOKUnixNano.Load() }

// LagBytes — оценка недоставленных байт WAL на конец последнего тика.
func (s *Shipper) LagBytes() int64 { return s.lagBytes.Load() }

// Start запускает фоновый цикл доставки.
func (s *Shipper) Start() {
	go s.run()
}

// Stop останавливает цикл и выполняет финальный тик: на graceful shutdown
// вызывается ПОСЛЕ bw.Close() (последний батч WAL уже на диске), поэтому
// штатная остановка даёт RPO=0 и на удалённой копии.
func (s *Shipper) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Shipper) run() {
	defer close(s.done)

	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()

	// Немедленный первый тик: догоняем backlog сразу, не через Interval.
	s.tickLogged()
	for {
		select {
		case <-ticker.C:
			s.tickLogged()
		case <-s.stop:
			s.tickLogged() // финальный тик — дошипливаем хвост
			return
		}
	}
}

func (s *Shipper) tickLogged() {
	if err := s.Tick(context.Background()); err != nil {
		shipErrorsInc()
		if !s.errState.Swap(true) {
			slog.Error("ship: доставка не удалась (повтор каждый тик)", "err", err)
		}
		return
	}
	if s.errState.Swap(false) {
		slog.Info("ship: доставка восстановлена")
	}
}

// Tick — один полный проход доставки. Экспортирован для детерминированных
// тестов; конкурентный вызов с работающим Start недопустим.
//
// Порядок внутри тика подчинён инварианту публикации (см. Manifest):
//  1. лениво инициализировать состояние из последнего удалённого манифеста;
//  2. дошипить хвосты всех локальных wal_*.log;
//  3. загрузить изменившиеся снапшоты (иммутабельные версии);
//  4. если набор файлов стабилен и все неактивные WAL полные — опубликовать
//     манифест; иначе тихо ждать следующего тика (старый манифест валиден);
//  5. при смене снапшота — ретеншн.
func (s *Shipper) Tick(ctx context.Context) error {
	var initErr error
	s.initOnce.Do(func() { initErr = s.loadState(ctx) })
	if initErr != nil {
		// Ошибка инициализации не должна залипнуть навсегда — позволяем повторить.
		s.initOnce = sync.Once{}
		return initErr
	}

	walsBefore, err := s.listLocalWALs()
	if err != nil {
		return err
	}

	// 2. Хвосты WAL. Файл может исчезнуть под ногами (компакция) — не ошибка,
	// его куски останутся в манифесте до следующей публикации.
	changed := false
	for _, name := range walsBefore {
		shipped, err := s.shipWALTail(ctx, name)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		changed = changed || shipped
	}

	// 3. Снапшоты (иммутабельны после rename → грузим целиком новую версию).
	snapChanged, err := s.shipSnapshotFile(ctx, snapshotFile, &s.snapshot)
	if err != nil {
		return err
	}
	graphChanged, err := s.shipSnapshotFile(ctx, graphFile, &s.graph)
	if err != nil {
		return err
	}
	changed = changed || snapChanged || graphChanged

	// 4. Публикация — только на стабильном наборе файлов.
	walsAfter, err := s.listLocalWALs()
	if err != nil {
		return err
	}
	s.updateLag(walsAfter)
	if !equalStrings(walsBefore, walsAfter) {
		// Компакция посреди тика (ротация/удаление старых WAL). Не публикуем:
		// возможно, снапшот уже новый, а хвост удалённого WAL не дошиплен.
		// Прошлый манифест остаётся валидной restore-точкой.
		return nil
	}
	if !changed && s.published {
		return nil
	}

	m, ready := s.buildManifest(walsAfter)
	if !ready {
		return nil // неактивный WAL ещё не дошиплен — публикация отложена
	}
	if err := publishManifest(ctx, s.remote, m); err != nil {
		return err
	}
	s.gen = m.Gen
	s.published = true
	s.lastOKUnixNano.Store(time.Now().UnixNano())

	// 5. Ретеншн — только при смене снапшота (именно тогда появляются
	// объекты-сироты: куски WAL-файлов, выпавших из манифеста после компакции).
	if snapChanged || graphChanged {
		if err := s.retention(ctx); err != nil {
			// Незавершённый ретеншн = лишние объекты, не потеря данных.
			slog.Warn("ship: ретеншн не завершён (продолжим при следующей компакции)", "err", err)
		}
	}
	return nil
}

// loadState восстанавливает состояние доставки из последнего манифеста —
// рестарт сервера не приводит к повторной загрузке уже доставленного.
func (s *Shipper) loadState(ctx context.Context) error {
	m, err := loadLatestManifest(ctx, s.remote)
	if err != nil {
		return fmt.Errorf("ship: load remote state: %w", err)
	}
	if m == nil {
		return nil // пустое хранилище
	}
	s.gen = m.Gen
	s.snapshot = m.Snapshot
	s.graph = m.Graph
	for i := range m.WALs {
		w := m.WALs[i]
		s.wals[w.Name] = &w
	}
	s.published = true
	slog.Info("ship: продолжаем с удалённого манифеста", "gen", m.Gen, "wal_files", len(m.WALs))
	return nil
}

// listLocalWALs — отсортированные имена wal_*.log в каталоге данных.
func (s *Shipper) listLocalWALs() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dataDir, "wal_*.log"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	sort.Strings(names)
	return names, nil
}

// shipWALTail дошипливает появившийся хвост файла. Возвращает true, если
// что-то загружено. Ошибка os.IsNotExist пробрасывается как есть (гонка с
// компакцией — вызывающий пропускает файл).
func (s *Shipper) shipWALTail(ctx context.Context, name string) (bool, error) {
	f, err := os.Open(filepath.Join(s.dataDir, name))
	if err != nil {
		return false, err
	}
	defer f.Close()

	st, ok := s.wals[name]
	if !ok {
		st = &WALFile{Name: name}
		s.wals[name] = st
	}

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < st.Size {
		// Append-only инвариант нарушен (файл усечён?) — никогда не должно
		// случиться. Кричим и не трогаем: перезапись кусков сломала бы уже
		// опубликованные манифесты.
		return false, fmt.Errorf("ship: %s: локальный размер %d < доставленного %d (файл усечён?)",
			name, info.Size(), st.Size)
	}

	shipped := false
	buf := make([]byte, 0)
	for st.Size < info.Size() {
		n := min(info.Size()-st.Size, maxChunkSize)
		if int64(cap(buf)) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := f.ReadAt(buf, st.Size); err != nil && err != io.EOF {
			return shipped, fmt.Errorf("ship: read %s@%d: %w", name, st.Size, err)
		}
		key := fmt.Sprintf("%s%s/%016x.chunk", walDirKey, name, st.Size)
		if err := s.remote.Put(ctx, key, buf); err != nil {
			return shipped, err
		}
		st.Chunks = append(st.Chunks, Chunk{Key: key, Offset: st.Size, Size: n})
		st.Size += n
		shipBytesAdd(n)
		shipped = true
	}
	return shipped, nil
}

// shipSnapshotFile загружает файл-снапшот, если его fingerprint изменился.
// Файл иммутабелен после атомарного rename; открытый fd даёт консистентное
// содержимое, даже если путь заменён новой версией во время чтения.
func (s *Shipper) shipSnapshotFile(ctx context.Context, name string, ref **FileRef) (bool, error) {
	f, err := os.Open(filepath.Join(s.dataDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // снапшота ещё не было (до первой компакции)
		}
		return false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	fp := fmt.Sprintf("%016x-%016x", info.Size(), info.ModTime().UnixNano())
	if *ref != nil && (*ref).Fingerprint == fp {
		return false, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("ship: read %s: %w", name, err)
	}
	key := joinKey(snapDirKey, fp, name)
	if err := s.remote.Put(ctx, key, data); err != nil {
		return false, err
	}
	shipBytesAdd(int64(len(data)))
	*ref = &FileRef{Key: key, Size: int64(len(data)), Fingerprint: fp}
	return true, nil
}

// buildManifest собирает манифест из текущего состояния. ready=false, если
// какой-то НЕАКТИВНЫЙ (не последний по имени) WAL загружен не полностью —
// публикация с ним нарушила бы инвариант restore-точки.
func (s *Shipper) buildManifest(localWALs []string) (*Manifest, bool) {
	m := &Manifest{
		Gen:             s.gen + 1,
		CreatedUnixNano: time.Now().UnixNano(),
		Snapshot:        s.snapshot,
		Graph:           s.graph,
	}
	for i, name := range localWALs {
		st, ok := s.wals[name]
		if !ok {
			// Файл появился между шагами тика — дождёмся следующего.
			return nil, false
		}
		if i < len(localWALs)-1 {
			if info, err := os.Stat(filepath.Join(s.dataDir, name)); err != nil || info.Size() != st.Size {
				return nil, false // неактивный WAL недошиплен (или исчез)
			}
		}
		m.WALs = append(m.WALs, *st)
	}
	sortWALs(m.WALs)
	return m, true
}

// retention удаляет манифесты старше последних RetainManifests и все
// data-объекты, на которые не ссылается ни один удерживаемый манифест.
func (s *Shipper) retention(ctx context.Context) error {
	keys, err := s.remote.List(ctx, "")
	if err != nil {
		return err
	}

	// Отбираем удерживаемые генерации.
	var gens []uint64
	for _, k := range keys {
		if gen, ok := parseManifestGen(k); ok {
			gens = append(gens, gen)
		}
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] > gens[j] })
	keep := map[uint64]struct{}{}
	for i, g := range gens {
		if i < s.opts.RetainManifests {
			keep[g] = struct{}{}
		}
	}

	// Живое множество = объекты всех удерживаемых манифестов.
	live := map[string]struct{}{manifestLatestKey: {}}
	for g := range keep {
		data, err := s.remote.Get(ctx, manifestKey(g))
		if err != nil {
			return err
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("ship: manifest gen=%d corrupt: %w", g, err)
		}
		for k := range m.liveObjects() {
			live[k] = struct{}{}
		}
	}

	for _, k := range keys {
		if _, ok := live[k]; ok {
			continue
		}
		// Чужие/служебные ключи вне нашей раскладки не трогаем.
		if !strings.HasPrefix(k, walDirKey) && !strings.HasPrefix(k, snapDirKey) && !strings.HasPrefix(k, manifestDirKey) {
			continue
		}
		if err := s.remote.Delete(ctx, k); err != nil {
			return err
		}
	}

	// Подчищаем состояние в памяти: куски файлов, отсутствующих в живом
	// множестве, больше не нужны (их нет ни локально, ни в манифестах).
	for name, st := range s.wals {
		if len(st.Chunks) > 0 {
			if _, ok := live[st.Chunks[0].Key]; !ok {
				delete(s.wals, name)
			}
		}
	}
	return nil
}

// updateLag пересчитывает суммарный недошипленный хвост локальных WAL.
func (s *Shipper) updateLag(localWALs []string) {
	var lag int64
	for _, name := range localWALs {
		info, err := os.Stat(filepath.Join(s.dataDir, name))
		if err != nil {
			continue
		}
		var shipped int64
		if st, ok := s.wals[name]; ok {
			shipped = st.Size
		}
		if info.Size() > shipped {
			lag += info.Size() - shipped
		}
	}
	s.lagBytes.Store(lag)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
