package vector

import (
	"kvstore/kvstore/internal/monitoring"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Внутренние методы
// =============================================================================

// ─── Build Pool: producer-consumer типы ───────────────────────────────────────

// taskKind — тип задачи компакции.
type taskKind int

const (
	taskFlushDelta  taskKind = iota // flush delta → новый L0 сегмент
	taskMergeLevel                  // merge N сегментов уровня L → сегмент уровня L+1
	taskConsolidate                 // idle-консолидация: merge ВСЕХ сегментов всех уровней в один
)

// compactionTask — единица работы. Создаётся coordinator'ом (GRAB под lock),
// обрабатывается build/merge goroutine (BUILD вне lock), результат возвращается
// через resultChan → coordinator атомарно применяет (PUBLISH под lock).
type compactionTask struct {
	kind      taskKind
	entries   []DeltaEntry // для taskFlushDelta: векторы из дельты
	segs      []segment    // для taskMergeLevel: сегменты для слияния (oldSegs для замены)
	targetLvl int          // куда публиковать результат (0 для flush, L+1 для merge)
	sourceLvl int          // для merge: уровень источника (L) — для cleanup oldSegs
	epoch     uint64       // эпоха на момент GRAB (для PUBLISH-time check)
	dim       int          // dim, захваченный под локом (build читает его вне лока)
	// flushDelta — сфлашиваемая дельта (taskFlushDelta): остаётся searchable в
	// lvs.flushing, пока applyResult не опубликует построенный из неё сегмент.
	flushDelta *DeltaSegment
	// result — заполнается goroutine после BUILD, читается coordinator при PUBLISH.
	result segment
}

// collectSegments возвращает flat срез всех активных сегментов (под RLock).
func (lvs *LeveledVectorStore) collectSegments() []segment {
	var segs []segment
	for _, level := range lvs.levels {
		segs = append(segs, level...)
	}
	return segs
}

// triggerCompact посылает сигнал в non-blocking канал.
// Вызывать под write lock.
func (lvs *LeveledVectorStore) triggerCompact() {
	select {
	case lvs.compactCh <- struct{}{}:
	default: // уже есть сигнал, не блокируем
	}
}

// compactWorker (coordinator) — ЧИСТЫЙ event loop. Никогда не строит сам.
//
//	select {
//	  compactCh  → grab delta → запустить goroutine build → продолжить
//	  resultChan → атомарно применить результат в levels[] → продолжить
//	}
//
// Старые сегменты НЕ убираются из levels[] пока merge не завершён (нет дыр для search).
// mergeInFlight[L] = true → merge уровня L в работе (новый не запускаем).
func (lvs *LeveledVectorStore) compactWorker() {
	// Idle-детект: тик = четверть окна затишья → консолидация стартует с
	// запаздыванием не более 25% от IdleConsolidateAfter. nil-канал (фича
	// выключена) в select блокируется вечно — ветка мертва без оверхеда.
	var idleC <-chan time.Time
	if lvs.cfg.IdleConsolidateAfter > 0 {
		t := time.NewTicker(lvs.cfg.IdleConsolidateAfter / 4)
		defer t.Stop()
		idleC = t.C
	}
	for {
		select {
		case <-lvs.done:
			return

		case <-lvs.compactCh:
			// Сигнал: возможно нужно что-то сделать (delta fill или merge overflow).
			lvs.handleCompactSignal()

		case r := <-lvs.resultChan:
			// Результат от build/merge goroutine → атомарно применить.
			lvs.applyResult(r)

		case <-idleC:
			// Затишье мутаций → флаш остатка дельты, затем консолидация сегментов.
			lvs.handleIdleTick()
		}
	}
}

// handleIdleTick — idle-консолидация (IdleConsolidateAfter). Вызывается только
// из compactWorker (тот же goroutine, что и handleCompactSignal/applyResult —
// вся координация однопоточна, флаги in-flight гоняться не могут).
//
// Последовательность (по тику за шаг, без ожиданий внутри):
//  1. Есть остаток дельты (не Full — сам не сфлашится никогда) → штатный flush
//     через triggerCompact; консолидация подождёт следующего тика.
//  2. Дельта пуста, работ в полёте нет, сегментов >1 и крупнейший держит <90%
//     данных → merge всех сегментов всех уровней в один.
func (lvs *LeveledVectorStore) handleIdleTick() {
	if time.Now().UnixNano()-lvs.lastMutation.Load() < int64(lvs.cfg.IdleConsolidateAfter) {
		return
	}

	lvs.mu.Lock()
	epoch := lvs.compactionEpoch.Load()

	if lvs.delta != nil && lvs.delta.Len() > 0 {
		lvs.mu.Unlock()
		lvs.triggerCompact() // handleCompactSignal сфлашит непустую дельту
		return
	}
	// Ждём тишины и в компакционной машинерии: параллельная консолидация с
	// flush/merge либо дублировала бы данные, либо мержила снапшот без хвоста.
	if lvs.consolidateInFlight || lvs.deltaFlushInFlight || lvs.inFlightBuilds.Load() > 0 || lvs.anyMergeInFlightLocked() {
		lvs.mu.Unlock()
		return
	}

	// Собираем сегменты СТАРШИЕ→МЛАДШИЕ (уровни сверху вниз, внутри уровня — по
	// созданию): дедуп в mergeSegments оставляет ПОСЛЕДНЮЮ копию ключа, порядок
	// oldest→newest обязателен для корректного upsert (как в maybeScheduleMerges).
	var toMerge []segment
	highest := 0
	for lyr := maxLevels - 1; lyr >= 0; lyr-- {
		if len(lvs.levels[lyr]) > 0 && highest == 0 {
			highest = lyr
		}
		toMerge = append(toMerge, lvs.levels[lyr]...)
	}
	if len(toMerge) <= 1 {
		lvs.mu.Unlock()
		return
	}
	// Guard от рестройки-шторма: почти всё уже в одном сегменте → полный rebuild
	// ради хвоста не окупается (капля в 1k векторов после паузы пересобирала бы
	// миллионный индекс). Хвост мелких L0 дочистит обычный fanout-каскад.
	total, largest := 0, 0
	for _, s := range toMerge {
		n := s.Len()
		total += n
		if n > largest {
			largest = n
		}
	}
	if total == 0 || largest*10 >= total*9 {
		lvs.mu.Unlock()
		return
	}

	lvs.consolidateInFlight = true
	targetLvl := highest + 1
	if targetLvl >= maxLevels {
		targetLvl = maxLevels - 1
	}
	lvs.mu.Unlock()

	// Merge-горутина — зеркало maybeScheduleMerges. Консолидация стартует только
	// при полном отсутствии merge в полёте, а новые merge блокируются
	// consolidateInFlight.
	go func(segs []segment, tgtLvl int, e uint64) {
		mergeStart := time.Now()
		var seg segment
		var applied map[string]struct{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					seg = nil
					applied = nil
				}
			}()
			seg, applied = lvs.mergeSegments(segs)
		}()
		monitoring.VectorMergeDuration.Update(time.Since(mergeStart).Seconds())
		res := compactionResult{
			kind:              taskConsolidate,
			targetLvl:         tgtLvl,
			epoch:             e,
			result:            seg,
			oldSegs:           segs,
			appliedTombstones: applied,
		}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
		}
	}(toMerge, targetLvl, epoch)
}

// anyMergeInFlightLocked — как anyMergeInFlight, но caller уже держит lvs.mu.
func (lvs *LeveledVectorStore) anyMergeInFlightLocked() bool {
	for i := 0; i < maxLevels; i++ {
		if lvs.mergeInFlight[i] {
			return true
		}
	}
	return false
}

// handleCompactSignal — обработка сигнала compactCh.
// GRAB delta → запустить goroutine build → проверить merges.
func (lvs *LeveledVectorStore) handleCompactSignal() {
	monitoring.VectorCompactionTotal.Inc()
	epoch := lvs.compactionEpoch.Load()

	// GRAB delta: атомарный swap под write-lock (микросекунды).
	lvs.mu.Lock()
	if lvs.delta == nil || lvs.delta.Len() == 0 {
		lvs.mu.Unlock()
		lvs.maybeScheduleMerges(epoch)
		return
	}
	// Кап flushing-бэклога: очередной swap при полном бэклоге лишь плодит
	// fp32-графы, которые каждый поиск обходит с ef-полом. Дельта копит дальше;
	// swap повторится с applyResult ближайшего build (re-check delta.Full()) или
	// со следующего пробуждения FlushDeltaSync. Жёсткий предел роста — гарантия,
	// что при перегрузе вставками build остаётся ограниченного размера.
	if len(lvs.flushing) >= flushingBacklogCap &&
		lvs.delta.Len() < deltaHardGrowthFactor*lvs.delta.MaxSize() {
		lvs.mu.Unlock()
		monitoring.VectorFlushSwapDeferredTotal.Inc()
		lvs.maybeScheduleMerges(epoch)
		return
	}
	oldDelta := lvs.delta
	// dim захватываем ПОД локом: build-горутина иначе читает lvs.dim без синхронизации,
	// а Clear() пишет lvs.dim=0 (pre-existing data race, обнажён Clear-during-build).
	dim := lvs.dim
	max := deltaMax(lvs.dim, lvs.cfg.DeltaMax)
	lvs.delta = NewDeltaSegmentSharded(lvs.dim, max, lvs.cfg.Distance, lvs.cfg.M, lvs.cfg.EfConstruction, lvs.deltaShardCount())
	// Держим сфлашиваемую дельту searchable до публикации сегмента (закрывает
	// flush-visibility gap). Close/удаление — в applyResult под mu.Lock.
	lvs.flushing = append(lvs.flushing, oldDelta)
	lvs.mu.Unlock()

	monitoring.VectorFlushDeltaTotal.Inc()
	if oldDelta.Len() > 0 {
		// Freeze-on-flush: графы дельты УЖЕ построены инкрементально при Add —
		// замораживаем их напрямую (SQ8 при UseSQ — любой dim; иначе CSR float32
		// при dim ≤ порога), без повторной сборки HNSW (нет double-build).
		// Тенант-режим (TenantOf/PartitionAttr) и атрибуты требуют rebuild для
		// сортировки/колонок → freeze (заморозка инкрементального графа как есть)
		// несовместим: он сохранил бы временной порядок вставки, а не тенант-порядок.
		// HasText — как HasAttrs: freeze заморозил бы граф БЕЗ segmentText, и
		// SEARCHTEXT, работавший по дельте, молча слеп бы после flush.
		canFreeze := lvs.cfg.TenantOf == nil && lvs.cfg.PartitionAttr == "" &&
			!oldDelta.HasAttrs() && !oldDelta.HasText() &&
			(lvs.cfg.UseSQ || lvs.dim <= csrDimThreshold)
		switch {
		case canFreeze && !oldDelta.Sharded():
			// Один шард → один граф → один сегмент. Close аллокатора — внутри
			// горутины ПОСЛЕ freeze (freeze копирует в Go-память).
			lvs.startFreezeDeltaGoroutine(oldDelta, epoch)
		case canFreeze:
			// Freeze-per-shard: каждый из nShards графов замораживается СВОИМ
			// сегментом → nShards мини-сегментов публикуются атомарно в L0, а
			// merge-каскад склеивает их фоном. Это схлопывает flushing-состояние
			// за миллисекунды вместо десятков секунд rebuild'а (563-2738×, см.
			// freeze_pershard_profit_test.go) → дорогие fp32-графы исчезают из
			// переходного состояния, бэклог не копится, кап swap'ов не срабатывает.
			// RepairReachability тут НЕ зовём: verify стоит ~полсборки (см.
			// graph.go), а достижимость мини = достижимость тех же графов, по
			// которым поиск ходил в дельте только что; первый же merge пере-
			// строит с шафлом+repair (buildSegment).
			lvs.startFreezeShardsGoroutine(oldDelta, epoch)
		default:
			// Freeze неприменим (attrs/tenant: нужна пересортировка entries;
			// fp32 dim>порога: frozen-CSR не применим) → rebuild из slab.
			// entries ссылаются на slab oldDelta (не на граф) → oldDelta должна жить до
			// конца build → Close откладывается в applyResult (после публикации сегмента),
			// а не сразу после ExtractAll (иначе UAF по slab при конкурентном Search).
			entries := oldDelta.ExtractAll()
			lvs.startBuildGoroutine(&compactionTask{
				kind:       taskFlushDelta,
				entries:    entries,
				targetLvl:  0,
				epoch:      epoch,
				dim:        dim,
				flushDelta: oldDelta,
			})
		}
	} else {
		// Пустая дельта (defensive; early-return выше отсекает Len==0): нечего строить.
		// Убираем из flushing и закрываем сразу.
		lvs.removeFlushing(oldDelta)
		oldDelta.Close()
		// Дельта снята с flushing → будим FlushDeltaSync (ждёт по членству).
		lvs.mu.Lock()
		lvs.flushCond.Broadcast()
		lvs.mu.Unlock()
	}

	// Проверить: нужно ли запустить merge (без блокировки coordinator).
	lvs.maybeScheduleMerges(epoch)
}

// removeFlushing удаляет сфлашенную дельту из lvs.flushing (берёт mu.Lock).
// Вызывается из build/freeze-горутин на shutdown-пути (lock не удержан).
func (lvs *LeveledVectorStore) removeFlushing(d *DeltaSegment) {
	if d == nil {
		return
	}
	lvs.mu.Lock()
	lvs.removeFlushingLocked(d)
	lvs.mu.Unlock()
}

// removeFlushingLocked удаляет d из lvs.flushing С СОХРАНЕНИЕМ ПОРЯДКА (порядок =
// свежесть для провенанс-дедупа). Вызывать под mu.Lock. Сдвиг in-place безопасен:
// search копирует срез flushing под RLock, а мутация идёт под эксклюзивным Lock.
func (lvs *LeveledVectorStore) removeFlushingLocked(d *DeltaSegment) {
	for i, f := range lvs.flushing {
		if f == d {
			lvs.flushing = append(lvs.flushing[:i], lvs.flushing[i+1:]...)
			return
		}
	}
}

// startBuildGoroutine запускает goroutine для BUILD delta-flush сегмента.
// inFlightBuilds.Add(1) выполняется СИНХРОННО до запуска горутины —
// FlushDeltaSync никогда не увидит ложный 0.
//
// Архитектура:
//   - buildSem семафор ограничивает CPU-нагрузку: ≤ numBuilders горутин одновременно.
//   - Граф сборки — свой у каждой горутины (связи в его собственной арене):
//     общего изменяемого состояния у параллельных сборок нет.
func (lvs *LeveledVectorStore) startBuildGoroutine(task *compactionTask) {
	// Синхронный инкремент ДО запуска горутины — критично для FlushDeltaSync.
	lvs.inFlightBuilds.Add(1)
	epoch := task.epoch
	flushDelta := task.flushDelta
	go func() {
		// Ожидаем свободный слот (семафор). Горутина блокируется, coordinator — нет.
		select {
		case lvs.buildSem <- struct{}{}:
			defer func() { <-lvs.buildSem }()
		case <-lvs.done:
			// Shutdown: coordinator уже вышел → applyResult не отработает.
			// Закрываем сфлашиваемую дельту сами (единственный owner Close на этом пути).
			lvs.removeFlushing(flushDelta)
			flushDelta.Close()
			res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: nil}
			select {
			case lvs.resultChan <- res:
			default:
				lvs.inFlightBuilds.Add(-1)
			}
			return
		}

		var seg segment
		func() {
			defer func() {
				if r := recover(); r != nil {
					seg = nil // паника → rollback
				}
			}()
			seg = lvs.buildSegment(task.entries, task.dim)
		}()

		// flushDelta передаётся в результат — applyResult удалит её из flushing и
		// Close'ит ПОСЛЕ публикации сегмента (держали searchable весь build).
		res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: seg, flushDelta: flushDelta}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
			// Shutdown до применения результата: coordinator не отработает — Close сами.
			lvs.removeFlushing(flushDelta)
			flushDelta.Close()
			lvs.inFlightBuilds.Add(-1)
		}
	}()
}

// startFreezeDeltaGoroutine замораживает УЖЕ построенный граф дельты напрямую
// в frozen-CSR (без rebuild HNSW). Применяется при dim ≤ csrDimThreshold.
// inFlightBuilds.Add(1) синхронно ДО запуска горутины (как startBuildGoroutine).
func (lvs *LeveledVectorStore) startFreezeDeltaGoroutine(oldDelta *DeltaSegment, epoch uint64) {
	lvs.inFlightBuilds.Add(1)
	go func() {
		select {
		case lvs.buildSem <- struct{}{}:
			defer func() { <-lvs.buildSem }()
		case <-lvs.done:
			// Shutdown: coordinator уже вышел → Close сами (единственный owner).
			lvs.removeFlushing(oldDelta)
			oldDelta.Close()
			res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: nil}
			select {
			case lvs.resultChan <- res:
			default:
				lvs.inFlightBuilds.Add(-1)
			}
			return
		}

		var seg segment
		func() {
			defer func() {
				if r := recover(); r != nil {
					seg = nil // паника → rollback
				}
			}()
			keys := oldDelta.FreezeKeys()
			g := oldDelta.Graph()
			if lvs.cfg.UseSQ {
				if fg := FreezeGraphSQ(g, lvs.cfg.Metric, keys); fg != nil {
					seg = &frozenSQSegment{fg: fg}
				}
			} else {
				if fg := FreezeGraph(g, lvs.cfg.Distance, keys); fg != nil {
					seg = &frozenSegment{fg: fg}
				}
			}
		}()
		// freeze скопировал данные в Go-память, НО oldDelta остаётся searchable в
		// flushing до публикации сегмента → Close откладывается в applyResult
		// (единственный owner Close на нормальном пути; эксклюзивно к search-RLock).
		res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: seg, flushDelta: oldDelta}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
			lvs.inFlightBuilds.Add(-1)
		}
	}()
}

// startFreezeShardsGoroutine — freeze-per-shard: замораживает КАЖДЫЙ шард
// шардированной дельты его собственным FreezeGraphSQ/FreezeGraph → nShards
// мини-сегментов (миллисекунды vs десятки секунд rebuild). Мини публикуются
// applyResult'ом атомарно (results) вместе со снятием дельты с flushing;
// merge-каскад склеивает их фоном (mergeSegments перестраивает с шафлом+repair).
//
// Семантика отказа — all-or-nothing, как у build: если хоть один непустой шард
// не заморозился (panic/nil), НИЧЕГО не публикуем (results=nil → rollback,
// частичная публикация потеряла бы данные шарда молча; WAL реплеит на рестарте).
// Пустые шарды (хэш-дисбаланс на маленькой дельте) пропускаются — это не отказ.
//
// Контракты как у startFreezeDeltaGoroutine: inFlightBuilds.Add(1) синхронно ДО
// горутины; buildSem занимаем ОДИН слот на весь freeze (предохранитель при
// перегрузе — freeze конкурирует за CPU наравне с builds, см. 17663b6); после
// swap дельта immutable → чтение графов/nodeKey без shard.mu безопасно; freeze
// копирует в Go-память → Close дельты (аллокаторов) в applyResult после публикации.
func (lvs *LeveledVectorStore) startFreezeShardsGoroutine(oldDelta *DeltaSegment, epoch uint64) {
	lvs.inFlightBuilds.Add(1)
	go func() {
		select {
		case lvs.buildSem <- struct{}{}:
			defer func() { <-lvs.buildSem }()
		case <-lvs.done:
			// Shutdown: coordinator уже вышел → Close сами (единственный owner).
			lvs.removeFlushing(oldDelta)
			oldDelta.Close()
			res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: nil}
			select {
			case lvs.resultChan <- res:
			default:
				lvs.inFlightBuilds.Add(-1)
			}
			return
		}

		nShards := oldDelta.ShardCount()
		minis := make([]segment, nShards) // [i] = мини шарда i; nil = пустой шард
		failed := atomic.Bool{}
		var wg sync.WaitGroup
		for i := 0; i < nShards; i++ {
			if oldDelta.ShardLiveLen(i) == 0 {
				continue // пустой шард — нечего замораживать (не отказ)
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						failed.Store(true) // паника шарда → rollback всего flush
					}
				}()
				keys := oldDelta.ShardFreezeKeys(i)
				g := oldDelta.ShardGraph(i)
				if lvs.cfg.UseSQ {
					if fg := FreezeGraphSQ(g, lvs.cfg.Metric, keys); fg != nil {
						minis[i] = &frozenSQSegment{fg: fg}
						return
					}
				} else {
					if fg := FreezeGraph(g, lvs.cfg.Distance, keys); fg != nil {
						minis[i] = &frozenSegment{fg: fg}
						return
					}
				}
				failed.Store(true) // непустой шард дал nil-freeze → rollback
			}(i)
		}
		wg.Wait()

		var results []segment
		if !failed.Load() {
			for _, m := range minis {
				if m != nil {
					results = append(results, m)
				}
			}
		}
		res := compactionResult{kind: taskFlushDelta, epoch: epoch, results: results, flushDelta: oldDelta}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
			lvs.removeFlushing(oldDelta)
			oldDelta.Close()
			lvs.inFlightBuilds.Add(-1)
		}
	}()
}

// maybeScheduleMerges проверяет переполненные уровни и запускает merge goroutines.
// По одному merge на уровень одновременно (mergeInFlight[L]). Сегменты НЕ убираются
// из levels[] — старые остаются видимы для search до замены.
func (lvs *LeveledVectorStore) maybeScheduleMerges(epoch uint64) {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	lvs.maybeScheduleMergesLocked(epoch)
}

// maybeScheduleMergesLocked — тело maybeScheduleMerges; caller держит lvs.mu.
// Вызывается напрямую из applyResult: каскад merge пинается БЕЗ triggerCompact,
// т.к. сигнал заставил бы handleCompactSignal сначала забрать текущую дельту,
// какой бы маленькой она ни была → при живом потоке вставок это
// самоподдерживающийся флаш-шторм (e2e 09.07: 424 микросегмента по ~140
// векторов на 60k вставок, пока долгий merge держал mergeInFlight[0]).
func (lvs *LeveledVectorStore) maybeScheduleMergesLocked(epoch uint64) {
	fanout := lvs.cfg.Fanout

	// Идёт консолидация — она уже мержит ВСЕ сегменты; параллельный merge тех же
	// сегментов опубликовал бы данные дважды (дубли в памяти под дедупом поиска).
	if lvs.consolidateInFlight {
		return
	}

	// Приоритет флаш-builds над merge (brownout после bulk-load, e2e 09.07):
	// каждая flushing-дельта — это nShards fp32-графов, которые КАЖДЫЙ поиск
	// обходит с ef-полом (70% CPU окна brownout — именно этот поиск). Merge
	// работает над frozen-сегментами, уже дешёвыми для поиска, и может подождать —
	// CPU отдаём builds, ликвидирующим дорогое состояние. Отложенный merge не
	// теряется: applyResult последнего build вызывает нас же, когда flushing пуст.
	deferMerges := len(lvs.flushing) > 0 || lvs.inFlightBuilds.Load() > 0

	for lyr := 0; lyr < maxLevels-1; lyr++ {
		if lvs.mergeInFlight[lyr] {
			continue // merge этого уровня уже в работе
		}
		if len(lvs.levels[lyr]) < fanout {
			continue // уровень не переполнен — продолжаем проверять выше (L1 может быть переполнен без L0)
		}
		// Предохранитель от голодания: под непрерывным insert-потоком flushing не
		// пустеет никогда — уровень, разросшийся до 2×fanout, мержим несмотря на
		// builds, иначе fanout поиска по уровню растёт без границы.
		if deferMerges && len(lvs.levels[lyr]) < 2*fanout {
			monitoring.VectorMergeDeferredTotal.Inc()
			continue
		}
		if lvs.compactionEpoch.Load() != epoch {
			break // Clear() — стоп
		}
		// Batch-merge: берём ВЕСЬ накопившийся префикс уровня до капа, а не ровно
		// fanout (см. LeveledConfig.MergeBatchMax — почему каскад по fanout штук
		// проигрывает freeze-per-shard гонку). Префикс = oldest→newest порядок,
		// обязательный для дедупа upsert в mergeSegments. Сегменты НЕ
		// удаляются из levels (copy slice header) — старые видимы для search до
		// applyResult.
		batch := len(lvs.levels[lyr])
		if batch > lvs.cfg.MergeBatchMax {
			batch = lvs.cfg.MergeBatchMax
		}
		toMerge := make([]segment, batch)
		copy(toMerge, lvs.levels[lyr][:batch])

		lvs.mergeInFlight[lyr] = true
		sourceLvl := lyr
		targetLvl := lyr + 1
		taskEpoch := epoch
		// Запускаем merge goroutine (coordinator не блокируется).
		go func(segs []segment, srcLvl, tgtLvl int, e uint64) {
			mergeStart := time.Now()
			var seg segment
			var applied map[string]struct{}
			func() {
				defer func() {
					if r := recover(); r != nil {
						seg = nil
						applied = nil
					}
				}()
				seg, applied = lvs.mergeSegments(segs)
			}()
			monitoring.VectorMergeDuration.Update(time.Since(mergeStart).Seconds())
			res := compactionResult{
				kind:              taskMergeLevel,
				sourceLvl:         srcLvl,
				targetLvl:         tgtLvl,
				epoch:             e,
				result:            seg,
				oldSegs:           segs,
				appliedTombstones: applied,
			}
			select {
			case lvs.resultChan <- res:
			case <-lvs.done:
			}
		}(toMerge, sourceLvl, targetLvl, taskEpoch)
	}
}

// applyResult — атомарное применение результата build/merge под write lock.
// Для flushDelta: просто append в levels[0].
// Для mergeLevel: убрать oldSegs из levels[source], добавить newSeg в levels[target].
// После применения: сброс mergeInFlight, re-check overflow, signal FlushDeltaSync.
func (lvs *LeveledVectorStore) applyResult(r compactionResult) {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Epoch check: если Clear() сработал во время build — результат устарел.
	if lvs.compactionEpoch.Load() != r.epoch {
		monitoring.VectorCompactionEpochMismatchTotal.Inc()
		if r.kind == taskFlushDelta {
			// Сегмент устарел (Clear) → не публикуем. Дельту всё равно снимаем с
			// searchable-списка и закрываем (owner Close на этом пути — здесь).
			lvs.removeFlushingLocked(r.flushDelta)
			if r.flushDelta != nil {
				r.flushDelta.Close()
			}
			lvs.inFlightBuilds.Add(-1)
			// Дельта снята с flushing → будим FlushDeltaSync (ждёт по членству).
			lvs.flushCond.Broadcast()
		} else if r.kind == taskConsolidate {
			lvs.consolidateInFlight = false
		} else {
			lvs.mergeInFlight[r.sourceLvl] = false
		}
		return
	}

	switch r.kind {
	case taskFlushDelta:
		lvs.inFlightBuilds.Add(-1)
		if len(r.results) > 0 {
			// Freeze-per-shard: все мини-сегменты шардов публикуются ОДНИМ append
			// под этим же Lock — search никогда не видит «половину дельты».
			lvs.levels[0] = append(lvs.levels[0], r.results...)
		} else if r.result != nil {
			lvs.levels[0] = append(lvs.levels[0], r.result)
		} else {
			monitoring.VectorCompactionRollbackTotal.Inc()
		}
		// Публикация сегмента и снятие дельты с searchable-списка — под одним Lock:
		// для любого Search переход delta→segment атомарен (видит либо flushing-дельту,
		// либо уже сегмент — но не «ни там, ни там» = закрытый flush-visibility gap).
		// Close ПОСЛЕ снятия из flushing и публикации: с этого момента дельту никто не
		// ищет (search берёт RLock, эксклюзивно к нашему Lock) → нет UAF по slab/графу.
		lvs.removeFlushingLocked(r.flushDelta)
		if r.flushDelta != nil {
			r.flushDelta.Close()
		}
		// Дельта снята с flushing и сегмент опубликован → будим FlushDeltaSync
		// (он перепроверит членство pre-call набора в flushing). Безусловно: waiter
		// сам решает, все ли его дельты опубликованы — здесь гейта быть не должно.
		lvs.flushCond.Broadcast()

	case taskMergeLevel:
		lvs.mergeInFlight[r.sourceLvl] = false
		if r.result == nil {
			// Merge не удался — сегменты остаются в levels (не трогаем), просто выходим.
			monitoring.VectorCompactionRollbackTotal.Inc()
			return
		}
		// Убираем oldSegs из levels[source] — они заменены новым сегментом.
		lvs.removeSegments(r.sourceLvl, r.oldSegs)
		// Добавляем новый сегмент в levels[target].
		lvs.levels[r.targetLvl] = append(lvs.levels[r.targetLvl], r.result)
		// Чистим tombstones применённых ключей — АТОМАРНО со свапом и лишь тех,
		// кого больше нет в других сегментах (см. clearAppliedTombstonesLocked).
		lvs.clearAppliedTombstonesLocked(r.appliedTombstones, r.result)

	case taskConsolidate:
		lvs.consolidateInFlight = false
		if r.result == nil {
			monitoring.VectorCompactionRollbackTotal.Inc()
			return
		}
		// oldSegs пришли со ВСЕХ уровней — чистим каждый (removeSegments фильтрует
		// по identity, лишние элементы в toRemove безвредны).
		for lyr := range lvs.levels {
			lvs.removeSegments(lyr, r.oldSegs)
		}
		lvs.levels[r.targetLvl] = append(lvs.levels[r.targetLvl], r.result)
		lvs.clearAppliedTombstonesLocked(r.appliedTombstones, r.result)
	}

	// Re-check каскада — ПРЯМЫМ вызовом под уже удержанным mu, НЕ через
	// triggerCompact: любой сигнал заставляет handleCompactSignal сначала
	// забрать текущую дельту (какой бы маленькой она ни была) → при живом
	// потоке вставок post-flush сигнал становится самоподдерживающимся
	// флаш-штормом (микросегменты по ~сотне векторов, e2e 09.07). Прямой вызов
	// планирует только merge переполненных уровней и дельту не трогает.
	// Закрывает и bulk-load→тишина: последний flush, публикующий fanout-ный L0,
	// сам ставит merge (раньше каскад пинался только СЛЕДУЮЩИМ flush).
	lvs.maybeScheduleMergesLocked(r.epoch)

	// Сигнал (с забором дельты) — ТОЛЬКО когда она реально ПОЛНА (safety на
	// случай пропущенного Add-триггера). Прежде здесь была ветка
	// `flushPending && deltaNonEmpty` — она заставляла applyResult гнать flush
	// остатка дельты после КАЖДОГО build во время FlushDeltaSync → flush-storm
	// (тысячи flush по 1-2 вектора, каждый — свежий дельта-сегмент, сотни ГБ
	// churn). FlushDeltaSync теперь сам форсит нужный swap и ждёт по членству в
	// flushing, поэтому eager-re-flush остатка здесь больше не нужен.
	if lvs.delta != nil && lvs.delta.Full() {
		lvs.triggerCompact()
	}
}

// removeSegments удаляет указанные сегменты из levels[lyr].
// O(N) но N маленькое (fanout сегментов на уровне).
func (lvs *LeveledVectorStore) removeSegments(lyr int, toRemove []segment) {
	if len(toRemove) == 0 {
		return
	}
	// Build set of pointers to remove (identity comparison).
	level := lvs.levels[lyr]
	kept := level[:0] // in-place filter
	for _, s := range level {
		found := false
		for _, r := range toRemove {
			if s == r { // pointer identity
				found = true
				break
			}
		}
		if !found {
			kept = append(kept, s)
		}
	}
	lvs.levels[lyr] = kept
}

// buildWorker — пуловый воркер delta-flush задач. Получает задачу из buildQueue,
// строит сегмент, применяет результат напрямую в levels[0].
func (lvs *LeveledVectorStore) buildWorker() {
	for {
		select {
		case <-lvs.done:
			return
		case task, ok := <-lvs.buildQueue:
			if !ok {
				return
			}
			// Build: паника перехватывается, seg=nil = rollback.
			var seg segment
			func() {
				defer func() {
					if r := recover(); r != nil {
						seg = nil
					}
				}()
				seg = lvs.buildSegment(task.entries, lvs.dim)
			}()

			// Применяем результат под write-lock и декрементируем счётчик DOPO применения.
			// Важно: декремент DOPO применения, чтобы FlushDeltaSync не вернулся раньше записи данных.
			lvs.mu.Lock()
			if lvs.compactionEpoch.Load() == task.epoch && seg != nil {
				lvs.levels[0] = append(lvs.levels[0], seg)
			} else if seg == nil {
				monitoring.VectorCompactionRollbackTotal.Inc()
			}
			// Декремент счётчика DOPO применения результата.
			lvs.inFlightBuilds.Add(-1)
			// Дельта снята с flushing → будим FlushDeltaSync (ждёт по членству).
			lvs.flushCond.Broadcast()
			overflow := len(lvs.levels[0]) >= lvs.cfg.Fanout
			// Re-flush ТОЛЬКО когда дельта реально полна (без flushPending-шторма,
			// как в applyResult). Примечание: этот путь (buildQueue/buildWorker)
			// сейчас не активен — enqueueTask не вызывается; держим консистентным на
			// случай будущего включения пула.
			needsFlush := !overflow && lvs.delta != nil && lvs.delta.Full()
			lvs.mu.Unlock()
			if overflow || needsFlush {
				lvs.triggerCompact()
			}
		}
	}
}

// FlushDeltaSync гарантирует: после возврата все векторы, добавленные ДО вызова,
// находятся в опубликованных сегментах (levels[]). Merge goroutines (L1→L2 и т.д.)
// НЕ ждём — они работают в фоне.
//
// ВАЖНО (fix flush-storm): ждём публикации лишь PRE-CALL набора дельт, а НЕ полного
// опустошения дельты. Прежняя реализация крутилась пока delta не станет ПУСТОЙ И
// inFlightBuilds==0 в один момент — под непрерывной конкурентной записью это
// никогда не сходилось: applyResult пере-триггерил flush на каждом build'е, дельта
// флашилась тысячи раз по 1-2 вектора, каждый flush аллоцировал свежий
// дельта-сегмент → сотни ГБ churn (см. soak-профиль 06.07). Контракт требует лишь
// «pre-call векторы в сегментах»; их держат:
//   - target: дельта на момент входа (форсим один swap, ждём её сегмент);
//   - pending: дельты, уже находящиеся в flushing на входе (их builds в полёте).
//
// Векторы, добавленные писателями ПОСЛЕ swap, попадают в НОВУЮ дельту — в набор не
// входят, их не ждём (они durable в WAL, уедут в следующий снапшот). Это и убивает
// шторм: конечный набор ожидания вместо гонки за движущейся пустотой.
//
// flushCond.Broadcast() вызывается из applyResult() при снятии дельты с flushing и
// из Close() при graceful shutdown; FlushDeltaSync пробуждается и перепроверяет.
func (lvs *LeveledVectorStore) FlushDeltaSync() {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Pre-call набор: текущая непустая дельта + уже флашащиеся дельты.
	var target *DeltaSegment
	if lvs.delta != nil && lvs.delta.Len() > 0 {
		target = lvs.delta
	}
	pending := append([]*DeltaSegment(nil), lvs.flushing...)

	inFlushing := func(d *DeltaSegment) bool {
		for _, f := range lvs.flushing {
			if f == d {
				return true
			}
		}
		return false
	}

	for {
		// Shutdown: coordinator уже вышел, ждать некому.
		select {
		case <-lvs.done:
			return
		default:
		}

		// target опубликован, когда он больше не активная дельта И снят с flushing.
		targetDone := target == nil || (lvs.delta != target && !inFlushing(target))
		pendingDone := true
		for _, d := range pending {
			if inFlushing(d) {
				pendingDone = false
				break
			}
		}
		if targetDone && pendingDone {
			// Всё pre-call — в сегментах. Толкнём каскад merge (L0→L1→L2)
			// напрямую, как applyResult, а НЕ через triggerCompact: сигнал
			// заставил бы координатор сначала забрать текущую дельту, а в ней
			// уже могут быть записи, сделанные ПОСЛЕ этого FlushDeltaSync, —
			// они уехали бы лишним мини-сегментом (тот же шторм, что описан у
			// maybeScheduleMergesLocked). Наблюдалось: upsert сразу после
			// FlushDeltaSync попадал во второй сегмент, и старая копия факта
			// всплывала в текстовом поиске, где сегмент-vs-сегмент гасится лишь
			// дедупом хитов (страж — TestFlushDeltaSyncDoesNotFlushLaterWrites).
			lvs.maybeScheduleMergesLocked(lvs.compactionEpoch.Load())
			return
		}

		// Форсим swap target'а — но ТОЛЬКО пока он ещё активная дельта. После swap
		// повторный триггер флашил бы уже НОВЫЕ дельты = прежний шторм.
		if target != nil && lvs.delta == target {
			lvs.triggerCompact()
		}
		lvs.flushCond.Wait()
	}
}

// anyMergeInFlight проверяет, есть ли активный merge на любом уровне.
func (lvs *LeveledVectorStore) anyMergeInFlight() bool {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	return lvs.anyMergeInFlightLocked()
}

// flushDeltaToSegment удалён — логика delta-flush интегрирована в coordinateCompaction
// (Build Pool). Coordinator забирает delta и создаёт task{taskFlushDelta}, buildWorker
// строит сегмент через buildSegment.

// mergeSegments сливает сегменты в один: снимает их записи (без tombstoned,
// с дедупликацией upsert) и строит новый сегмент через buildSegment.
func (lvs *LeveledVectorStore) mergeSegments(segs []segment) (segment, map[string]struct{}) {
	lvs.mu.RLock()
	dim := lvs.dim
	// Snapshot tombstones под RLock — tombstones актуальны на момент начала merge.
	// Ключи, удалённые до merge, не попадают в новый сегмент.
	// Ключи, удалённые после snapshot, останутся в новом сегменте — они будут
	// в tombstones и отфильтруются при следующем Search/Get. Корректно.
	var ts map[string]struct{}
	if t := lvs.tombstones.Load(); t != nil {
		ts = *t
	}
	lvs.mu.RUnlock()

	if dim == 0 {
		return nil, nil
	}

	// applied — ключи, исключённые из нового сегмента по tombstone. Кандидаты на
	// очистку tombstone, но чистим их НЕ здесь, а в applyResult после атомарной
	// публикации сегмента и только если ключа больше нет в других сегментах.
	applied := make(map[string]struct{})

	// Извлекаем векторы, пропуская tombstoned ключи, и ДЕДУПЛИЦИРУЕМ по ключу.
	// Один ключ может жить в двух merge-входах (upsert после flush: свежая копия
	// уехала в новый сегмент, старая осталась в прежнем; оба попадают в один merge).
	// segs идут oldest→newest (caller берёт префикс levels[lyr][:batch] в порядке создания),
	// поэтому более поздняя запись СВЕЖЕЕ и перезаписывает раннюю — так stale-копия
	// физически выметается из индекса (иначе она осталась бы дублем ВНУТРИ сегмента,
	// который провенанс-дедуп поиска уже не различит — одинаковый ранг).
	var entries []DeltaEntry
	seen := make(map[string]int)
	addEntry := func(key string, vec []float32, attrs Attributes, terms []TermTF) {
		if pos, ok := seen[key]; ok {
			entries[pos] = DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms}
			return
		}
		seen[key] = len(entries)
		entries = append(entries, DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms})
	}
	// Текст (шаг 6): постинги сегмента декодируются в пер-доковые термы ОДИН раз
	// на входной сегмент (decodeTerms — обратный проход buildSegmentText), едут в
	// entries и пересобираются build'ом заново. Слияние листов напрямую невозможно:
	// frozen id перенумеровываются, tombstoned/затенённые копии выпадают, тенант-
	// сорт меняет порядок. Побочный эффект пересборки — нормализация статистики:
	// df/N/avgdl завышенные затенёнными копиями возвращаются к правде (конец
	// «принятой семантики» шага 4). Все входы без текста → termsAt даёт nil →
	// buildSegmentText вернёт nil (zero overhead сохраняется).
	termsAt := func(dec [][]TermTF, i int) []TermTF {
		if i < len(dec) {
			return dec[i]
		}
		return nil
	}
	for _, seg := range segs {
		switch s := seg.(type) {
		case *frozenSegment:
			fg := s.fg
			decTerms := s.text.decodeTerms()
			for i := 0; i < fg.n; i++ {
				if fg.keys.view(i) == "" {
					continue
				}
				if ts != nil {
					if _, deleted := ts[fg.keys.view(i)]; deleted {
						applied[fg.keys.clone(i)] = struct{}{}
						continue // tombstone — физически удаляем
					}
				}
				vec := fg.data[i*fg.dim : (i+1)*fg.dim]
				vecCopy := make([]float32, dim)
				copy(vecCopy, vec)
				addEntry(fg.keys.clone(i), vecCopy, s.attrs.decodeAt(i), termsAt(decTerms, i))
			}
		case *frozenSQSegment:
			// Деквантуем SQ8-векторы обратно в float32 для rebuild.
			// Это единственная операция, теряющая точность: деквантованный вектор
			// ≈ исходный с точностью SQ8 (error ≤ scale/2 per dimension).
			// При rebuild новый сегмент квантуется заново — накопления ошибки нет.
			fg := s.fg
			decTerms := s.text.decodeTerms()
			for i := 0; i < fg.n; i++ {
				if fg.keys.view(i) == "" {
					continue
				}
				if ts != nil {
					if _, deleted := ts[fg.keys.view(i)]; deleted {
						applied[fg.keys.clone(i)] = struct{}{}
						continue
					}
				}
				vecCopy := make([]float32, dim)
				base := i * dim
				for d := 0; d < dim; d++ {
					vecCopy[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
				}
				addEntry(fg.keys.clone(i), vecCopy, s.attrs.decodeAt(i), termsAt(decTerms, i))
			}
		case *hnswSegment:
			s.mu.RLock()
			g := s.g
			// node id = frozen id текстового слоя (build без шафла) — тот же
			// индекс, что у attrs.decodeAt.
			decTerms := s.text.decodeTerms()
			for i := range g.nodes {
				if !g.nodes[i].Alive {
					continue
				}
				key, ok := s.keys[uint64(i)]
				if !ok || key == "" {
					continue
				}
				if ts != nil {
					if _, deleted := ts[key]; deleted {
						applied[key] = struct{}{}
						continue // tombstone — физически удаляем
					}
				}
				raw := g.arena.Get(g.nodes[i].VectorOffset)
				vecCopy := make([]float32, dim)
				copy(vecCopy, raw)
				addEntry(key, vecCopy, s.attrs.decodeAt(i), termsAt(decTerms, i))
			}
			s.mu.RUnlock()
		}
	}

	if len(entries) == 0 {
		// Все ключи входа tombstoned. Сегмент не публикуется (applyResult сделает
		// rollback: oldSegs остаются в levels) → ключи физически живы → tombstones
		// НЕ чистим. Возвращаем applied=nil, чтобы clearAppliedTombstonesLocked не
		// сработал (он и так гейтован r.result != nil).
		return nil, nil
	}

	newSeg := lvs.buildSegment(entries, dim)

	// Очистку применённых tombstones делает applyResult ПОСЛЕ атомарной публикации
	// сегмента (свап под mu.Lock) и только для ключей, отсутствующих в остальных
	// сегментах. Внутри merge-горутины НЕ чистим: (#3) при дропе результата (epoch-
	// mismatch/rollback) tombstone обязан сохраниться.
	return newSeg, applied
}

// buildSegment строит сегмент из набора (key, vec) записей.
// Dim-adaptive: dim ≤ csrDimThreshold → FrozenGraph, иначе → hnswSegment.
// Используется сборкой при flush дельты, слиянием и загрузкой hnsw-сегментов.
func (lvs *LeveledVectorStore) buildSegment(entries []DeltaEntry, dim int) segment {
	n := len(entries)
	if n == 0 {
		return nil
	}

	buildStart := time.Now()
	defer func() {
		monitoring.VectorSegmentBuildDuration.Update(time.Since(buildStart).Seconds())
	}()

	// Тенант-локальная раскладка (Вещь 1): #5 ведущий ключ компакции.
	// Сортируем entries по коду тенанта ДО вставки в граф → node id (= frozen id)
	// идут в тенант-порядке → каждый тенант непрерывным блоком. Затем строим
	// каталог диапазонов (#6 тип индекса по размеру) и вешаем на сегмент.
	var cat *TenantCatalog
	var attrs *segmentAttrs
	// presets переиспользует партиционный dict в колонке, если тенант-раскладка
	// строится (коды каталога обязаны совпасть с кодами колонки — иначе SearchFilter
	// не найдёт блок). Без партиции остаётся nil → колонки строят свои dict.
	var presets map[string]*attrDict
	if lvs.cfg.TenantOf != nil || lvs.cfg.PartitionAttr != "" {
		// Тенант-раскладка (Вещь 1): сортируем entries по коду тенанта и строим
		// каталог диапазонов. Источник кода: партиционный атрибут либо legacy-ключ.
		var tenantCode func(DeltaEntry) uint64
		if lvs.cfg.PartitionAttr != "" {
			pa := lvs.cfg.PartitionAttr
			pd := newAttrDict()
			tenantCode = func(e DeltaEntry) uint64 { return uint64(pd.intern(e.Attrs.Cat[pa])) }
			presets = map[string]*attrDict{pa: pd}
		} else {
			tenantCode = tenantByKey(lvs.cfg.TenantOf)
		}
		sortEntriesByTenant(entries, tenantCode)
		c, err := buildTenantCatalog(entries, tenantCode, lvs.cfg.BruteThreshold, dim)
		if err != nil {
			// Инвариант #5 нарушен (entries не сгруппированы после сортировки) —
			// это баг сортировки/деривации тенанта, не данных. Фейлим build:
			// coordinator откатит (seg=nil), сегмент не публикуется битым.
			monitoring.VectorSegmentBuildDuration.Update(0)
			return nil
		}
		cat = c
	}
	// Колоночный слой строим ВСЕГДА, когда в entries есть атрибуты — НЕЗАВИСИМО от
	// партиции. Иначе SearchFilter, работавший на свежих данных в дельте, молча
	// выпадал бы после flush (attrs жили только в дельте). buildSegmentAttrs
	// возвращает nil, если атрибутов нет (zero-overhead для чистых векторов).
	// entries к этому моменту уже отсортированы (если была тенант-раскладка) →
	// frozen id = позиция, колонки выровнены.
	attrs = buildSegmentAttrs(entries, presets)

	// Текстовый BM25-слой — тем же правилом, что attrs: всегда, когда в entries
	// есть термы (nil иначе — zero overhead). entries уже в финальном порядке →
	// постинги лягут по frozen id. Merge-путь кладёт Terms декодом постингов
	// входных сегментов (mergeSegments → decodeTerms).
	text := buildSegmentText(entries)

	// Строим HNSW из всех векторов.
	g := NewGraph(lvs.cfg.Distance)
	g.EfConstruction = lvs.cfg.EfConstruction

	// Применяем параметр M если задан
	if lvs.cfg.M > 0 {
		g.M = lvs.cfg.M
		g.M0 = 2 * lvs.cfg.M
		g.Ml = 1.0 / math.Log(float64(lvs.cfg.M))
		g.pruneBufItems = make([]item, 0, g.M0+1)
		g.pruneBufIDs = make([]uint64, 0, g.M0+1)
		g.insertBuf = make([]uint64, 0, g.M0+1)
	}
	// n известно заранее: арена связей растёт один раз, без копирований.
	g.reserveNodes(n)

	// Порядок ВСТАВКИ в граф перемешиваем (детерминированно по n): entries к этому
	// моменту блочно отсортированы (тенант-сорт выше; у клиента — батчи похожих
	// данных), а HNSW-построение деградирует на блочном порядке мультимодальных
	// данных: блоки разных распределений не образуют кросс-рёбер, и у части вершин
	// раннего блока beam-поиск позже не находит даже точное совпадение (soak
	// searchmiss 7/400 детерминированно; форензика 07.2026: miss 8/1007 при
	// блочном порядке vs 0/1007 при перемешанном на том же наборе). Раскладка
	// данных ОСТАЁТСЯ отсортированной — каталог тенантов и колоночные атрибуты
	// адресуют позицию в entries; граф переупорядочивается при freeze через pos.
	// Для hnswSegment (без freeze) порядок не трогаем: там graph id — источник
	// адресации, перестановка ломала бы каталог.
	willFreeze := lvs.cfg.UseSQ || dim <= csrDimThreshold
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	var pos []uint32
	if willFreeze && n > 1 {
		rand.New(rand.NewSource(0x5eed<<32|int64(n))).Shuffle(n, func(a, b int) {
			order[a], order[b] = order[b], order[a]
		})
		pos = make([]uint32, n)
	}

	keys := make(map[uint64]string, n)
	for _, ei := range order {
		idx := g.Insert(entries[ei].Vec)
		if pos != nil {
			pos[idx] = uint32(ei)
		}
		keys[uint64(idx)] = entries[ei].Key
	}

	// Repair-pass: детерминированная гарантия self-findability при рабочем
	// efSearch. Шафл выше снимает систематическую порядкозависимость, repair
	// добивает вероятностный хвост мультимодальной геометрии (см.
	// Graph.RepairReachability). Фоновый билд — бюджет CPU есть.
	g.RepairReachability(lvs.cfg.EfSearch)

	// Выбор формата сегмента (frozen uint32 neighbor IDs — потолка n нет):
	//   UseSQ=true → SQ8-frozen для ЛЮБОЙ размерности. На dim>256 это и есть решение
	//     памяти для 768 (4× сжатие: 3072→768 B/vec). SQ-гейт по ПОЛЬЗЕ, не по dim.
	//   UseSQ=false → float32-frozen только при dim ≤ csrDimThreshold (там CSR даёт
	//     +QPS за счёт cache-locality); иначе обычный hnswSegment.
	if lvs.cfg.UseSQ {
		fg := FreezeGraphSQOrdered(g, lvs.cfg.Metric, keys, pos)
		if fg == nil {
			return nil
		}
		return &frozenSQSegment{fg: fg, cat: cat, attrs: attrs, text: text}
	}
	if dim <= csrDimThreshold {
		fg := FreezeGraphOrdered(g, lvs.cfg.Distance, keys, pos)
		if fg == nil {
			return nil
		}
		return &frozenSegment{fg: fg, cat: cat, attrs: attrs, text: text}
	}

	return &hnswSegment{g: g, keys: keys, cat: cat, attrs: attrs, text: text}
}
