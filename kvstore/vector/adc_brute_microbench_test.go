package vector

import (
	"math"
	"testing"
)

// =============================================================================
// Микробенч ДО кода для спринта «fused ADC на brute-путях» (бэклог
// vector-brute-sq8-adc-backlog-20260710, хедрум ~15× замерен e2e 11.07).
//
// Вопрос: сколько даёт замена боевого пути bruteRange/bruteRangeAttr
// (скалярный деквант-цикл в buf + float32 distFn вторым проходом) на fused
// AVX2 ADC-ядра (sq8EuclidImpl/sq8DotImpl), по метрикам и размерностям.
//
// Варианты:
//   A_scalar     — боевой путь как в frozen_sq.go:212-215 (деквант + distFn)
//   B_fused      — sq8EuclidImpl / 1-sq8DotImpl (drop-in для euclid/dot)
//   C_fused2pass — cosine из ДВУХ существующих ядер: dot = sq8DotImpl(q,codes),
//                  |approx|² = sq8EuclidImpl(нулевой query, codes) = Σ approx².
//                  Без нового ассемблера; codes горячие в L1 после 1-го прохода.
//   D_twoAccGo   — cosine одним pure-Go проходом с двумя аккумуляторами
//                  (нижняя планка будущего единого asm-ядра dot+norm).
//
// Слаб = 4096 векторов (residual-brute масштаб: matched ~2.5k на dbpedia);
// при 1536d это 6МБ codes — L3-resident, как тёплый сегмент.
// =============================================================================

const adcBenchN = 4096

type adcBenchData struct {
	codes   []uint8
	sqMin   []float32
	sqScale []float32
	query   []float32
	zeroQ   []float32 // нулевой query для трюка |approx|² через sq8EuclidImpl
	dim     int
}

func makeADCBenchData(dim int, seed uint64) *adcBenchData {
	d := &adcBenchData{
		codes:   make([]uint8, adcBenchN*dim),
		sqMin:   make([]float32, dim),
		sqScale: make([]float32, dim),
		query:   make([]float32, dim),
		zeroQ:   make([]float32, dim),
		dim:     dim,
	}
	s := seed
	next := func() uint64 {
		s = s*6364136223846793005 + 1442695040888963407
		return s
	}
	for i := range d.codes {
		d.codes[i] = uint8(next() % 256)
	}
	for i := 0; i < dim; i++ {
		d.sqMin[i] = float32(int64(next())%200)/100.0 - 1.0 // [-1, 1)
		d.sqScale[i] = float32(next()%100)/12750.0 + 0.001  // ~(0, 0.0088)
		d.query[i] = float32(int64(next())%2000) / 1000.0   // [-1, 1)
	}
	// query нормируем — как реальные cosine-эмбеддинги
	Normalize(d.query)
	return d
}

// ── эталоны/варианты для одного вектора ──

// scalarDequantDist — боевой путь bruteRange: скалярный деквант + distFn.
func (d *adcBenchData) scalarDequantDist(i int, buf []float32, distFn DistanceFunc) float32 {
	base := i * d.dim
	for k := 0; k < d.dim; k++ {
		buf[k] = d.sqMin[k] + float32(d.codes[base+k])*d.sqScale[k]
	}
	return distFn(d.query, buf)
}

// cosineFused2Pass — cosine из двух существующих AVX2-ядер.
// invQNorm = 1/|query| предвычислен на запрос (1 раз на весь brute-скан).
func (d *adcBenchData) cosineFused2Pass(i int, invQNorm float32) float32 {
	base := i * d.dim
	cs := d.codes[base : base+d.dim]
	dot := sq8DotImpl(d.query, cs, d.sqMin, d.sqScale)
	normB2 := sq8EuclidImpl(d.zeroQ, cs, d.sqMin, d.sqScale) // Σ approx²
	if normB2 == 0 {
		return 1
	}
	return 1 - dot*invQNorm/float32(math.Sqrt(float64(normB2)))
}

// cosineTwoAccPureGo — один проход, два аккумулятора (планка единого asm-ядра).
func (d *adcBenchData) cosineTwoAccPureGo(i int, invQNorm float32) float32 {
	base := i * d.dim
	var dot, normB2 float32
	for k := 0; k < d.dim; k++ {
		approx := d.sqMin[k] + float32(d.codes[base+k])*d.sqScale[k]
		dot += d.query[k] * approx
		normB2 += approx * approx
	}
	if normB2 == 0 {
		return 1
	}
	return 1 - dot*invQNorm/float32(math.Sqrt(float64(normB2)))
}

// ── корректность: fused-варианты ≡ боевому скалярному пути ──

func TestADCBruteVariants_Correctness(t *testing.T) {
	for _, dim := range []int{128, 960, 1536} {
		d := makeADCBenchData(dim, uint64(dim)*7919)
		buf := make([]float32, dim)
		qn := float32(math.Sqrt(float64(dotProductSQ8ExactQNorm(d.query))))
		invQNorm := 1 / qn
		for i := 0; i < 64; i++ {
			// euclid: fused ≡ деквант+EuclideanDistance
			se := d.scalarDequantDist(i, buf, EuclideanDistance)
			fe := sq8EuclidImpl(d.query, d.codes[i*dim:(i+1)*dim], d.sqMin, d.sqScale)
			if !approxEqualRel(se, fe, 0.001) {
				t.Fatalf("dim=%d i=%d euclid: scalar=%v fused=%v", dim, i, se, fe)
			}
			// dot: fused ≡ деквант+DotProductDistance
			sd := d.scalarDequantDist(i, buf, DotProductDistance)
			fd := 1 - sq8DotImpl(d.query, d.codes[i*dim:(i+1)*dim], d.sqMin, d.sqScale)
			if !approxEqualRel(sd, fd, 0.001) {
				t.Fatalf("dim=%d i=%d dot: scalar=%v fused=%v", dim, i, sd, fd)
			}
			// cosine: оба fused-варианта ≡ деквант+CosineDistance
			sc := d.scalarDequantDist(i, buf, CosineDistance)
			c2 := d.cosineFused2Pass(i, invQNorm)
			cg := d.cosineTwoAccPureGo(i, invQNorm)
			// cosine-значения ~[0,2]; сравниваем абсолютно (rel у нуля шумит)
			if !approxEqual(sc, c2, 0.0005) {
				t.Fatalf("dim=%d i=%d cosine2pass: scalar=%v fused=%v", dim, i, sc, c2)
			}
			if !approxEqual(sc, cg, 0.0005) {
				t.Fatalf("dim=%d i=%d cosineTwoAcc: scalar=%v twoAcc=%v", dim, i, sc, cg)
			}
		}
	}
}

// dotProductSQ8ExactQNorm — |query|² тем же порядком суммирования, что CosineDistance.
func dotProductSQ8ExactQNorm(q []float32) float32 { return dotProductImpl(q, q) }

// TestBruteRangeFusedADC_MatchesScalar — интеграционный: bruteRange/bruteRangeAttr
// на fused-пути обязаны выдавать тот же top-K (ключи + скоры), что прежний
// скалярный «деквант+distFn», на всех трёх метриках.
func TestBruteRangeFusedADC_MatchesScalar(t *testing.T) {
	const n, K = 512, 10
	for _, tc := range []struct {
		name   string
		metric Metric
	}{
		{"euclid", MetricEuclidean},
		{"dot", MetricDot},
		{"cosine", MetricCosine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, dim := range []int{17, 128, 1536} { // 17 — с SIMD-remainder
				d := makeADCBenchData(dim, uint64(dim)*31337)
				keys := make([]string, n)
				for i := range keys {
					keys[i] = "k" + itoa(i)
				}
				keys[7] = "" // tombstone — должен пропускаться
				fg := &FrozenGraphSQ{
					codes:   d.codes[:n*dim],
					sqMin:   d.sqMin,
					sqScale: d.sqScale,
					keys:    buildInternedKeys(keys),
					n:       n,
					dim:     dim,
					distFn:  tc.metric.DistanceFunc(),
					dotMode: tc.metric.IsDot(),
					metric:  tc.metric,
				}

				// эталон: прежний скалярный путь
				want := make([]FrozenResult, 0, K)
				buf := make([]float32, dim)
				for i := 0; i < n; i++ {
					if keys[i] == "" {
						continue
					}
					base := i * dim
					for k := 0; k < dim; k++ {
						buf[k] = d.sqMin[k] + float32(d.codes[base+k])*d.sqScale[k]
					}
					want = insertTopK(want, K, keys[i], fg.distFn(d.query, buf))
				}

				got := fg.bruteRange(d.query, K, 0, n, nil, nil)
				checkTopKMatch(t, tc.name, dim, "bruteRange", got, want)

				// bruteRangeAttr с предикатом «чётные индексы»
				wantAttr := make([]FrozenResult, 0, K)
				for i := 0; i < n; i += 2 {
					if keys[i] == "" {
						continue
					}
					base := i * dim
					for k := 0; k < dim; k++ {
						buf[k] = d.sqMin[k] + float32(d.codes[base+k])*d.sqScale[k]
					}
					wantAttr = insertTopK(wantAttr, K, keys[i], fg.distFn(d.query, buf))
				}
				gotAttr := fg.bruteRangeAttr(d.query, K, 0, n, nil, func(i int) bool { return i%2 == 0 })
				checkTopKMatch(t, tc.name, dim, "bruteRangeAttr", gotAttr, wantAttr)
			}
		})
	}
}

func checkTopKMatch(t *testing.T, metric string, dim int, path string, got, want []FrozenResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s dim=%d %s: len got=%d want=%d", metric, dim, path, len(got), len(want))
	}
	for i := range got {
		if got[i].Key != want[i].Key {
			t.Errorf("%s dim=%d %s: rank %d key got=%q want=%q (got dist=%v want=%v)",
				metric, dim, path, i, got[i].Key, want[i].Key, got[i].Dist, want[i].Dist)
			continue
		}
		if !approxEqual(got[i].Dist, want[i].Dist, 0.001) {
			t.Errorf("%s dim=%d %s: rank %d dist got=%v want=%v",
				metric, dim, path, i, got[i].Dist, want[i].Dist)
		}
	}
}

// ── бенчмарки: op = скан всего слаба (adcBenchN векторов) ──

func benchScan(b *testing.B, dim int, perVec func(i int) float32) {
	b.Helper()
	var sink float32
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := 0; i < adcBenchN; i++ {
			sink += perVec(i)
		}
	}
	if sink == 0 {
		b.Log("sink=0")
	}
	nsPerVec := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(adcBenchN)
	b.ReportMetric(nsPerVec, "ns/vec")
	// пропускная по codes (байт = dim на вектор)
	gbps := float64(dim) / nsPerVec
	b.ReportMetric(gbps, "GB/s_codes")
}

func BenchmarkADCBrute(b *testing.B) {
	for _, dim := range []int{128, 960, 1536} {
		d := makeADCBenchData(dim, uint64(dim)*104729)
		buf := make([]float32, dim)
		qn := float32(math.Sqrt(float64(dotProductSQ8ExactQNorm(d.query))))
		invQNorm := 1 / qn

		b.Run(benchName("Euclid/A_scalar", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 { return d.scalarDequantDist(i, buf, EuclideanDistance) })
		})
		b.Run(benchName("Euclid/B_fused", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 {
				return sq8EuclidImpl(d.query, d.codes[i*dim:(i+1)*dim], d.sqMin, d.sqScale)
			})
		})

		b.Run(benchName("Dot/A_scalar", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 { return d.scalarDequantDist(i, buf, DotProductDistance) })
		})
		b.Run(benchName("Dot/B_fused", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 {
				return 1 - sq8DotImpl(d.query, d.codes[i*dim:(i+1)*dim], d.sqMin, d.sqScale)
			})
		})

		b.Run(benchName("Cosine/A_scalar", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 { return d.scalarDequantDist(i, buf, CosineDistance) })
		})
		b.Run(benchName("Cosine/C_fused2pass", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 { return d.cosineFused2Pass(i, invQNorm) })
		})
		b.Run(benchName("Cosine/D_twoAccGo", dim), func(b *testing.B) {
			benchScan(b, dim, func(i int) float32 { return d.cosineTwoAccPureGo(i, invQNorm) })
		})
	}
}

func benchName(variant string, dim int) string {
	return variant + "/dim" + itoa(dim)
}
