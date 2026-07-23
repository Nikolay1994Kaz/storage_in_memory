// vmemload — E2E-нагрузчик VMEM.* по RESP (шаг 8 VMEM, часть 2).
//
// Развилка А шага 8: качество судит store-бенч со сжатым временем
// (TestVMEMCorpusBench), латентность — ЭТОТ бинарник сквозь весь стек
// (RESP-парсинг → диспетчер/гейты → Recall/Remember → WAL/KV-якорь → сеть)
// на реальных часах. Оба конца обязаны питаться ОДНИМ генератором
// internal/vmemcorpus и ОДНИМ seed — иначе числа BENCHMARKS.md §7 будут
// «про разные миры».
//
// Фазы:
//  1. load   — реплей ленты корпуса (REMEMBER plain / REMEMBER+SUPERSEDES /
//     FORGET) с латентностями по классам. Порядок ленты сохраняется
//     пер-scope (воркер = hash(scope)): supersedes-цепочки живут внутри
//     scope, значит цель всегда ингестится раньше наследника. Факты, чей
//     TTL истёк бы к NowV ленты, НЕ ингестятся (симуляция уже отработавшего
//     жнеца — состояние ≈ store-бенч); живым TTL-фактам TTL не ставится
//     (TTL-семантика запинена store-уровнем, шаг 7: часы диспетчера не
//     инжектируются, реальный TTL исказил бы ленту).
//  2. recall — все запросы корпуса (шесть сортов) с явным ASOF NowV для
//     дефолт-режима (реальный now сервера ≠ виртуальный now ленты; контракт
//     «дефолт ≡ AS_OF now» делает это эквивалентностью, методика шага 7).
//     Латентности p50/p95/p99 по сортам + E2E-инвариант изоляции: каждый
//     возвращённый id обязан принадлежать scope запроса (ScopeOf корпуса).
//  3. mix    — soak-mix (долг чеклиста unfreeze, шаг 7 → шаг 8): читатели
//     гоняют случайные запросы корпуса, писатели льют свежие факты с
//     КОРОТКИМИ реальными TTL и FORGET'ами — WAL (включая op 9), гейты и
//     строгое затенение работают под смешанным давлением. Критерий: ноль
//     ошибок протокола, латентности не разъезжаются.
//
// Запуск (методика канона: один сервер за раз, чистый dataDir):
//
//	go build -o /tmp/kvstore ./kvstore/cmd/kvstore && (cd /tmp/kvdata && /tmp/kvstore -port 6390 -metrics-port 0)
//	go run ./kvstore/cmd/vmemload -addr 127.0.0.1:6390 -mix 60s
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/vmemcorpus"
)

// client — одно RESP-соединение.
type client struct {
	conn net.Conn
	w    *protocol.Writer
	r    *protocol.Reader
}

func dial(addr string) (*client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &client{conn: conn, w: protocol.NewWriter(conn), r: protocol.NewReader(conn)}, nil
}

func (c *client) do(args []protocol.Value) (protocol.Value, error) {
	if err := c.w.Write(protocol.Value{Typ: '*', Array: args}); err != nil {
		return protocol.Value{}, err
	}
	if err := c.w.Flush(); err != nil {
		return protocol.Value{}, err
	}
	v, err := c.r.Read()
	if err != nil {
		return protocol.Value{}, err
	}
	if v.Typ == '-' {
		return v, fmt.Errorf("server: %s", v.Str)
	}
	return v, nil
}

func bulk(s string) protocol.Value { return protocol.Value{Typ: '$', Str: s} }

// lats — накопитель латентностей одного класса операций (потокобезопасный
// через пер-воркер слайсы, merge в конце).
type lats struct {
	mu   sync.Mutex
	durs []time.Duration
	errs int64
}

func (l *lats) add(batch []time.Duration) {
	l.mu.Lock()
	l.durs = append(l.durs, batch...)
	l.mu.Unlock()
}

func (l *lats) report(name string, wall time.Duration) {
	slices.Sort(l.durs)
	n := len(l.durs)
	if n == 0 {
		fmt.Printf("%-22s n=0 errs=%d\n", name, l.errs)
		return
	}
	p := func(q float64) time.Duration { return l.durs[min(n-1, int(float64(n)*q))] }
	qps := ""
	if wall > 0 {
		qps = fmt.Sprintf("  QPS=%.0f", float64(n)/wall.Seconds())
	}
	fmt.Printf("%-22s n=%-6d errs=%-3d p50=%-10v p95=%-10v p99=%-10v max=%v%s\n",
		name, n, l.errs, p(0.50), p(0.95), p(0.99), l.durs[n-1], qps)
}

func rememberArgs(f *vmemcorpus.Fact, hybrid bool, ttlSec int64) []protocol.Value {
	args := []protocol.Value{bulk("VMEM.REMEMBER"), bulk(f.Scope), bulk("TEXT"), bulk(f.Text),
		bulk("ID"), bulk(f.ID), bulk("VALIDFROM"), bulk(strconv.FormatInt(f.At, 10))}
	if f.Type != "" {
		args = append(args, bulk("TYPE"), bulk(f.Type))
	}
	if f.Imp >= 0 {
		args = append(args, bulk("IMPORTANCE"), bulk(strconv.FormatFloat(f.Imp, 'f', -1, 64)))
	}
	if f.Supersedes != "" {
		args = append(args, bulk("SUPERSEDES"), bulk(f.Supersedes))
	}
	if ttlSec > 0 {
		args = append(args, bulk("TTL"), bulk(strconv.FormatInt(ttlSec, 10)))
	}
	if hybrid {
		args = append(args, bulk("VEC"))
		for _, v := range f.Vec {
			args = append(args, bulk(strconv.FormatFloat(float64(v), 'f', -1, 32)))
		}
	}
	return args
}

func recallArgs(q *vmemcorpus.Query, nowV int64, hybrid bool) []protocol.Value {
	args := []protocol.Value{bulk("VMEM.RECALL"), bulk(q.Scope), bulk(strconv.Itoa(q.K)), bulk(q.Text)}
	switch {
	case q.All:
		args = append(args, bulk("ALL"))
	case q.AsOf != 0:
		args = append(args, bulk("ASOF"), bulk(strconv.FormatInt(q.AsOf, 10)))
	default:
		// Дефолт-режим ленты: реальный now сервера ≠ виртуальный NowV,
		// «дефолт ≡ AS_OF now» превращает это в явный ASOF (методика шага 7).
		args = append(args, bulk("ASOF"), bulk(strconv.FormatInt(nowV, 10)))
	}
	if hybrid {
		args = append(args, bulk("VEC"))
		for _, v := range q.Vec {
			args = append(args, bulk(strconv.FormatFloat(float64(v), 'f', -1, 32)))
		}
	}
	return args
}

func main() {
	addr := flag.String("addr", "127.0.0.1:6390", "адрес сервера")
	workers := flag.Int("workers", 8, "параллельных соединений в фазах load/recall")
	seed := flag.Int64("seed", 42, "seed генератора (ОБЯЗАН совпадать со store-бенчем качества)")
	hybrid := flag.Bool("hybrid", false, "слать VEC (hashed-BoW корпуса) в REMEMBER/RECALL — гибридное плечо")
	mixDur := flag.Duration("mix", 0, "длительность soak-mix фазы (0 = пропустить)")
	mixWriters := flag.Int("mix-writers", 2, "писателей в mix-фазе (читатели = workers)")
	skipLoad := flag.Bool("skip-load", false, "не реплеить ленту (сервер уже загружен этим же seed)")
	flag.Parse()

	params := vmemcorpus.Default()
	params.Seed = *seed
	corpus := vmemcorpus.Generate(params)
	fmt.Printf("vmemload: %d событий, %d запросов, NowV=%d, hybrid=%v, workers=%d\n",
		len(corpus.Events), len(corpus.Queries), corpus.NowV, *hybrid, *workers)

	// --- Фаза 1: load ------------------------------------------------------
	classes := map[string]*lats{
		"remember":       {},
		"remember+super": {},
		"forget":         {},
	}
	var loadWall time.Duration
	if !*skipLoad {
		// Пер-scope шардирование: события одного scope идут одним воркером в
		// порядке ленты (supersedes-цель раньше наследника, FORGET после ингеста).
		shards := make([][]vmemcorpus.Event, *workers)
		for _, ev := range corpus.Events {
			scope := ev.ForgetScope
			if ev.Fact != nil {
				scope = ev.Fact.Scope
			}
			h := fnv.New32a()
			h.Write([]byte(scope))
			s := int(h.Sum32()) % *workers
			shards[s] = append(shards[s], ev)
		}
		var wg sync.WaitGroup
		t0 := time.Now()
		for wi := 0; wi < *workers; wi++ {
			wg.Add(1)
			go func(events []vmemcorpus.Event) {
				defer wg.Done()
				c, err := dial(*addr)
				if err != nil {
					fmt.Fprintln(os.Stderr, "dial:", err)
					os.Exit(1)
				}
				defer c.conn.Close()
				local := map[string][]time.Duration{}
				for i := range events {
					ev := &events[i]
					var class string
					var args []protocol.Value
					if ev.Fact != nil {
						// TTL-факт, мёртвый к NowV, не ингестится: состояние
						// сервера ≈ store-бенч после жнеца. Живому TTL не шлём
						// (реальные часы исказили бы ленту).
						if ev.Fact.TTLSec > 0 && ev.Fact.At+ev.Fact.TTLSec <= corpus.NowV {
							continue
						}
						class = "remember"
						if ev.Fact.Supersedes != "" {
							class = "remember+super"
						}
						args = rememberArgs(ev.Fact, *hybrid, 0)
					} else {
						class = "forget"
						args = []protocol.Value{bulk("VMEM.FORGET"), bulk(ev.ForgetScope), bulk(ev.ForgetID)}
					}
					t := time.Now()
					_, err := c.do(args)
					d := time.Since(t)
					if err != nil {
						atomic.AddInt64(&classes[class].errs, 1)
						continue
					}
					local[class] = append(local[class], d)
				}
				for cl, batch := range local {
					classes[cl].add(batch)
				}
			}(shards[wi])
		}
		wg.Wait()
		loadWall = time.Since(t0)
	}

	// --- Фаза 2: recall ----------------------------------------------------
	sorts := map[string]*lats{}
	for _, s := range []string{"known", "para", "asof", "nowchain", "order", "erasure"} {
		sorts[s] = &lats{}
	}
	var isoViolations, recallHits int64
	var qi int64
	var wg sync.WaitGroup
	t0 := time.Now()
	for wi := 0; wi < *workers; wi++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dial(*addr)
			if err != nil {
				fmt.Fprintln(os.Stderr, "dial:", err)
				os.Exit(1)
			}
			defer c.conn.Close()
			local := map[string][]time.Duration{}
			for {
				i := int(atomic.AddInt64(&qi, 1)) - 1
				if i >= len(corpus.Queries) {
					break
				}
				q := &corpus.Queries[i]
				args := recallArgs(q, corpus.NowV, *hybrid)
				t := time.Now()
				v, err := c.do(args)
				d := time.Since(t)
				if err != nil {
					atomic.AddInt64(&sorts[q.Sort].errs, 1)
					continue
				}
				local[q.Sort] = append(local[q.Sort], d)
				// E2E-инвариант изоляции: каждый id из выдачи — из scope запроса.
				for j := 0; j+2 < len(v.Array); j += 3 {
					id := v.Array[j].Str
					atomic.AddInt64(&recallHits, 1)
					if sc, ok := corpus.ScopeOf[id]; ok && sc != q.Scope {
						atomic.AddInt64(&isoViolations, 1)
					}
				}
			}
			for s, batch := range local {
				sorts[s].add(batch)
			}
		}()
	}
	wg.Wait()
	recallWall := time.Since(t0)

	// --- Фаза 3: mix (soak-mix) --------------------------------------------
	mixRead, mixWrite := &lats{}, &lats{}
	if *mixDur > 0 {
		var stop atomic.Bool
		var wg2 sync.WaitGroup
		for wi := 0; wi < *workers; wi++ {
			wg2.Add(1)
			go func(seed int64) {
				defer wg2.Done()
				c, err := dial(*addr)
				if err != nil {
					return
				}
				defer c.conn.Close()
				rng := rand.New(rand.NewSource(seed))
				var local []time.Duration
				for !stop.Load() {
					q := &corpus.Queries[rng.Intn(len(corpus.Queries))]
					t := time.Now()
					if _, err := c.do(recallArgs(q, corpus.NowV, *hybrid)); err != nil {
						atomic.AddInt64(&mixRead.errs, 1)
						continue
					}
					local = append(local, time.Since(t))
				}
				mixRead.add(local)
			}(int64(1000 + wi))
		}
		for wi := 0; wi < *mixWriters; wi++ {
			wg2.Add(1)
			go func(seed int64) {
				defer wg2.Done()
				c, err := dial(*addr)
				if err != nil {
					return
				}
				defer c.conn.Close()
				rng := rand.New(rand.NewSource(seed))
				var local []time.Duration
				n := 0
				for !stop.Load() {
					n++
					f := &vmemcorpus.Fact{
						ID:    fmt.Sprintf("mix:%d:%d", seed, n),
						Scope: fmt.Sprintf("mixuser:%03d", rng.Intn(50)),
						Type:  "note",
						Imp:   -1,
						At:    corpus.NowV,
						Text:  corpus.Queries[rng.Intn(len(corpus.Queries))].Text,
						Vec:   corpus.Queries[rng.Intn(len(corpus.Queries))].Vec,
					}
					// Короткие РЕАЛЬНЫЕ TTL: жнец и expires-фильтр под живым
					// миксом; часть фактов стирается FORGET'ом вручную.
					t := time.Now()
					_, err := c.do(rememberArgs(f, *hybrid, int64(5+rng.Intn(40))))
					if err != nil {
						atomic.AddInt64(&mixWrite.errs, 1)
						continue
					}
					local = append(local, time.Since(t))
					if n%7 == 0 {
						t = time.Now()
						if _, err := c.do([]protocol.Value{bulk("VMEM.FORGET"), bulk(f.Scope), bulk(f.ID)}); err != nil {
							atomic.AddInt64(&mixWrite.errs, 1)
						} else {
							local = append(local, time.Since(t))
						}
					}
				}
				mixWrite.add(local)
			}(int64(2000 + wi))
		}
		time.Sleep(*mixDur)
		stop.Store(true)
		wg2.Wait()
	}

	// --- Отчёт --------------------------------------------------------------
	fmt.Println("\n=== load (реплей ленты) ===")
	for _, cl := range []string{"remember", "remember+super", "forget"} {
		classes[cl].report(cl, loadWall)
	}
	fmt.Printf("\n=== recall (%d запросов, %d воркеров) ===\n", len(corpus.Queries), *workers)
	for _, s := range []string{"known", "para", "asof", "nowchain", "order", "erasure"} {
		sorts[s].report("recall/"+s, 0)
	}
	all := &lats{}
	for _, s := range sorts {
		all.durs = append(all.durs, s.durs...)
		all.errs += s.errs
	}
	all.report("recall/ALL", recallWall)
	fmt.Printf("изоляция scope (E2E-инвариант, обязан быть 0): %d нарушений на %d выданных id\n",
		isoViolations, recallHits)
	if *mixDur > 0 {
		fmt.Printf("\n=== mix (%v, %d читателей + %d писателей) ===\n", *mixDur, *workers, *mixWriters)
		mixRead.report("mix/recall", *mixDur)
		mixWrite.report("mix/write", *mixDur)
	}
	if isoViolations > 0 {
		os.Exit(1)
	}
}
