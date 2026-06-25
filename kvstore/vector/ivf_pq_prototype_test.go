package vector

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"
	"unsafe"
)

// loadSIFTRaw загружает SIFT данные из простого raw binary файла.
// Формат: [4B n_train][4B dim][n_train*dim*4B train][4B n_test][n_test*dim*4B test]
func loadSIFTRaw(path string) (train [][]float32, test [][]float32, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 8 {
		return nil, nil, fmt.Errorf("file too small")
	}

	n := int(binary.LittleEndian.Uint32(data[0:4]))
	dim := int(binary.LittleEndian.Uint32(data[4:8]))
	off := 8

	trainSize := n * dim * 4
	if off+trainSize > len(data) {
		return nil, nil, fmt.Errorf("train data truncated")
	}

	// Zero-copy: reinterpret float32 from byte slice.
	trainFlat := unsafe.Slice((*float32)(unsafe.Pointer(&data[off])), n*dim)
	train = make([][]float32, n)
	for i := 0; i < n; i++ {
		train[i] = trainFlat[i*dim : (i+1)*dim]
	}
	off += trainSize

	if off+4 > len(data) {
		return nil, nil, fmt.Errorf("test count missing")
	}
	nTest := int(binary.LittleEndian.Uint32(data[off : off+4]))
	off += 4

	testSize := nTest * dim * 4
	if off+testSize > len(data) {
		// Maybe no test set; return train only.
		return train, nil, nil
	}
	testFlat := unsafe.Slice((*float32)(unsafe.Pointer(&data[off])), nTest*dim)
	test = make([][]float32, nTest)
	for i := 0; i < nTest; i++ {
		test[i] = testFlat[i*dim : (i+1)*dim]
	}

	return train, test, nil
}

// =============================================================================
// IVF + PQ PROTOTYPE — standalone валидация БЕЗ изменения production кода.
//
// Цель: измерить recall/memory/QPS для IVF+PQ на SIFT-128 и синтетике dim=768.
// Если гипотезы подтверждаются — переносим в production.
// Если нет — удаляем файл, ничего не сломано.
// =============================================================================

// ─── K-means (стандартный, Lloyd's algorithm) ─────────────────────────────

func kmeans(vecs [][]float32, k int, iters int) [][]float32 {
	n := len(vecs)
	if n == 0 || k <= 0 {
		return nil
	}
	dim := len(vecs[0])
	rng := rand.New(rand.NewSource(42))

	// K-means++ init: выбираем начальные центроиды умно (не случайно).
	centroids := make([][]float32, k)
	centroids[0] = copyVec(vecs[rng.Intn(n)])
	dists := make([]float32, n)
	for i := range dists {
		dists[i] = EuclideanDistance(vecs[i], centroids[0])
	}
	for c := 1; c < k; c++ {
		// Выбираем точку пропорционально distance² (farthest-first).
		sum := float32(0)
		for _, d := range dists {
			sum += d
		}
		target := rng.Float32() * sum
		cumsum := float32(0)
		idx := 0
		for i, d := range dists {
			cumsum += d
			if cumsum >= target {
				idx = i
				break
			}
		}
		centroids[c] = copyVec(vecs[idx])
		// Update distances.
		for i := range vecs {
			d := EuclideanDistance(vecs[i], centroids[c])
			if d < dists[i] {
				dists[i] = d
			}
		}
	}

	// Lloyd's iterations.
	assignments := make([]int, n)
	for iter := 0; iter < iters; iter++ {
		// Assign step.
		changed := 0
		for i, v := range vecs {
			bestC := 0
			bestD := EuclideanDistance(v, centroids[0])
			for c := 1; c < k; c++ {
				d := EuclideanDistance(v, centroids[c])
				if d < bestD {
					bestD = d
					bestC = c
				}
			}
			if assignments[i] != bestC {
				assignments[i] = bestC
				changed++
			}
		}
		if changed == 0 && iter > 0 {
			break // converged
		}
		// Update step.
		sums := make([][]float64, k)
		counts := make([]int, k)
		for c := 0; c < k; c++ {
			sums[c] = make([]float64, dim)
		}
		for i, v := range vecs {
			c := assignments[i]
			for d := 0; d < dim; d++ {
				sums[c][d] += float64(v[d])
			}
			counts[c]++
		}
		for c := 0; c < k; c++ {
			if counts[c] > 0 {
				for d := 0; d < dim; d++ {
					centroids[c][d] = float32(sums[c][d] / float64(counts[c]))
				}
			}
		}
	}
	return centroids
}

// ─── IVF index (prototype) ────────────────────────────────────────────────

type ivfIndex struct {
	centroids [][]float32 // [nlist][dim]
	lists     [][]int     // [nlist] → vector indices
	vectors   [][]float32 // все векторы (для recall)
	nprobe   int
}

func trainIVF(vecs [][]float32, nlist int) *ivfIndex {
	centroids := kmeans(vecs, nlist, 20)
	idx := &ivfIndex{
		centroids: centroids,
		lists:     make([][]int, nlist),
		vectors:   vecs,
		nprobe:    1,
	}
	// Assign vectors to nearest centroid.
	for i, v := range vecs {
		c := idx.nearestCentroid(v)
		idx.lists[c] = append(idx.lists[c], i)
	}
	return idx
}

func (idx *ivfIndex) nearestCentroid(v []float32) int {
	bestC := 0
	bestD := EuclideanDistance(v, idx.centroids[0])
	for c := 1; c < len(idx.centroids); c++ {
		d := EuclideanDistance(v, idx.centroids[c])
		if d < bestD {
			bestD = d
			bestC = c
		}
	}
	return bestC
}

// searchTopNprobe находит nprobe ближайших кластеров к query.
func (idx *ivfIndex) searchTopNprobe(query []float32) []int {
	type cd struct {
		c   int
		dist float32
	}
	cds := make([]cd, len(idx.centroids))
	for c := range idx.centroids {
		cds[c] = cd{c, EuclideanDistance(query, idx.centroids[c])}
	}
	sort.Slice(cds, func(i, j int) bool { return cds[i].dist < cds[j].dist })
	nprobe := idx.nprobe
	if nprobe > len(cds) {
		nprobe = len(cds)
	}
	result := make([]int, nprobe)
	for i := 0; i < nprobe; i++ {
		result[i] = cds[i].c
	}
	return result
}

// searchExact — brute-force top-K в nprobe кластерах.
func (idx *ivfIndex) searchExact(query []float32, K int) []int {
	clusters := idx.searchTopNprobe(query)
	type vd struct {
		idx  int
		dist float32
	}
	var cands []vd
	for _, c := range clusters {
		for _, vecIdx := range idx.lists[c] {
			d := EuclideanDistance(query, idx.vectors[vecIdx])
			cands = append(cands, vd{vecIdx, d})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	if len(cands) > K {
		cands = cands[:K]
	}
	result := make([]int, len(cands))
	for i, c := range cands {
		result[i] = c.idx
	}
	return result
}

// ─── PQ index (prototype) ─────────────────────────────────────────────────

type pqIndex struct {
	M         int           // число подпространств
	dsub      int           // dim / M
	centroids [][][]float32 // [M][256][dsub]
	codes     [][]byte      // [n][M] — закодированные векторы
	dim       int
}

func trainPQ(vecs [][]float32, M int) *pqIndex {
	n := len(vecs)
	dim := len(vecs[0])
	dsub := dim / M
	pq := &pqIndex{M: M, dsub: dsub, dim: dim}

	// Для каждого подпространства: train 256 centroids.
	pq.centroids = make([][][]float32, M)
	for m := 0; m < M; m++ {
		// Извлекаем под-векторы для этого подпространства.
		subVecs := make([][]float32, n)
		for i := range vecs {
			subVecs[i] = make([]float32, dsub)
			copy(subVecs[i], vecs[i][m*dsub:(m+1)*dsub])
		}
		pq.centroids[m] = kmeans(subVecs, 256, 10)
	}

	// Encode all vectors.
	pq.codes = make([][]byte, n)
	for i := range vecs {
		pq.codes[i] = pq.encode(vecs[i])
	}
	return pq
}

func (pq *pqIndex) encode(vec []float32) []byte {
	code := make([]byte, pq.M)
	for m := 0; m < pq.M; m++ {
		sub := vec[m*pq.dsub : (m+1)*pq.dsub]
		bestC := 0
		bestD := EuclideanDistance(sub, pq.centroids[m][0])
		for c := 1; c < 256; c++ {
			d := EuclideanDistance(sub, pq.centroids[m][c])
			if d < bestD {
				bestD = d
				bestC = c
			}
		}
		code[m] = byte(bestC)
	}
	return code
}

// adcDistance — Asymmetric Distance Computation.
// query остаётся в float32, codes — в PQ. Для каждого подпространства
// precompute distance table [256], затем sum lookup.
func (pq *pqIndex) adcDistance(query []float32, code []byte) float32 {
	// Precompute distance tables per sub-space [256].
	// (В production это делается один раз per query, не per vector.)
	var total float32
	for m := 0; m < pq.M; m++ {
		sub := query[m*pq.dsub : (m+1)*pq.dsub]
		d := EuclideanDistance(sub, pq.centroids[m][code[m]])
		total += d
	}
	return total
}

// searchExact — brute-force top-K using PQ-ADC distances.
func (pq *pqIndex) searchExact(query []float32, K int) []int {
	type vd struct {
		idx  int
		dist float32
	}
	cands := make([]vd, len(pq.codes))
	for i := range pq.codes {
		cands[i] = vd{i, pq.adcDistance(query, pq.codes[i])}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	if len(cands) > K {
		cands = cands[:K]
	}
	result := make([]int, len(cands))
	for i, c := range cands {
		result[i] = c.idx
	}
	return result
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func copyVec(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	return out
}

func bruteForceTopKIdx(vecs [][]float32, query []float32, K int) []int {
	type vd struct {
		idx  int
		dist float32
	}
	all := make([]vd, len(vecs))
	for i, v := range vecs {
		all[i] = vd{i, EuclideanDistance(query, v)}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].dist < all[j].dist })
	result := make([]int, K)
	for i := 0; i < K && i < len(all); i++ {
		result[i] = all[i].idx
	}
	return result
}

func measureRecallIdx(found, truth []int) float64 {
	tSet := make(map[int]bool, len(truth))
	for _, t := range truth {
		tSet[t] = true
	}
	hits := 0
	for _, f := range found {
		if tSet[f] {
			hits++
		}
	}
	return float64(hits) / float64(len(truth))
}

// =============================================================================
// TESTS
// =============================================================================

// TestProtIVF_Recall — IVF на SIFT-128.
// Цель: nprobe=4 → recall ≥95%.
func TestProtIVF_Recall(t *testing.T) {
	if testing.Short() {
		t.Skip("IVF prototype test is slow")
	}
	const K = 10

	vecs, queries, err := loadSIFTRaw("/tmp/sift_raw.bin")
	if err != nil {
		t.Skipf("SIFT raw not found: %v (run convert_sift.py)", err)
	}
	n := len(vecs)
	dim := len(vecs[0])
	t.Logf("Loaded SIFT: %d vectors, dim=%d, %d queries", n, dim, len(queries))

	// Ground truth.
	gt := make([][]int, len(queries))
	for i, q := range queries {
		gt[i] = bruteForceTopKIdx(vecs, q, K)
	}

	for _, nlist := range []int{4, 16, 64, 256} {
		ivf := trainIVF(vecs, nlist)
		for _, nprobe := range []int{1, 2, 4, 8, 16} {
			if nprobe > nlist {
				continue
			}
			ivf.nprobe = nprobe
			totalRecall := 0.0
			for i, q := range queries {
				found := ivf.searchExact(q, K)
				totalRecall += measureRecallIdx(found, gt[i])
			}
			avgRecall := totalRecall / float64(len(queries)) * 100
			t.Logf("IVF nlist=%d nprobe=%d: recall=%.1f%% (cluster avg=%.0f vecs)",
				nlist, nprobe, avgRecall, float64(n)/float64(nlist))
		}
	}
}

// TestProtIVF_TrainTime — время k-means обучения.
func TestProtIVF_TrainTime(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	const n = 50000
	const dim = 128
	vecs := makeRandVecs(n, dim, 42)

	for _, nlist := range []int{16, 64, 256} {
		start := time.Now()
		_ = kmeans(vecs, nlist, 20)
		dur := time.Since(start)
		t.Logf("kmeans n=%d k=%d: %v", n, nlist, dur)
	}
}

// TestProtPQ_Recall — PQ на SIFT-128 (реальные данные).
// Цель: recall ≥90% при M=8-16.
func TestProtPQ_Recall(t *testing.T) {
	if testing.Short() {
		t.Skip("PQ prototype test is slow")
	}
	const K = 10

	vecs, queries, err := loadSIFTRaw("/tmp/sift_raw.bin")
	if err != nil {
		t.Skipf("SIFT raw not found: %v", err)
	}
	n := len(vecs)
	dim := len(vecs[0])

	gt := make([][]int, len(queries))
	for i, q := range queries {
		gt[i] = bruteForceTopKIdx(vecs, q, K)
	}

	for _, M := range []int{8, 16, 32} {
		pq := trainPQ(vecs, M)
		totalRecall := 0.0
		for i, q := range queries {
			found := pq.searchExact(q, K)
			totalRecall += measureRecallIdx(found, gt[i])
		}
		avgRecall := totalRecall / float64(len(queries)) * 100
		bytesPerVec := M
		memMB := float64(n*bytesPerVec) / 1e6
		t.Logf("PQ M=%d: recall=%.1f%% | %.0f B/vec | %.1f MB total (%.1f× compression)",
			M, avgRecall, float64(bytesPerVec), memMB,
			float64(n*dim*4)/float64(n*bytesPerVec))
	}
}

// TestProtPQ_Dim768 — PQ на dim=768 (синтетика).
// Цель: recall ≥90%, cluster ≤0.5 MB.
func TestProtPQ_Dim768(t *testing.T) {
	if testing.Short() {
		t.Skip("PQ dim768 test is slow")
	}
	const n = 20000 // меньше для скорости
	const dim = 768
	const K = 10

	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = make([]float32, dim)
		for d := range vecs[i] {
			vecs[i][d] = rng.Float32()*2 - 1
		}
	}
	queries := make([][]float32, 30)
	for i := range queries {
		queries[i] = make([]float32, dim)
		for d := range queries[i] {
			queries[i][d] = rng.Float32()*2 - 1
		}
	}

	gt := make([][]int, len(queries))
	for i, q := range queries {
		gt[i] = bruteForceTopKIdx(vecs, q, K)
	}

	for _, M := range []int{16, 32, 64} {
		pq := trainPQ(vecs, M)
		totalRecall := 0.0
		for i, q := range queries {
			found := pq.searchExact(q, K)
			totalRecall += measureRecallIdx(found, gt[i])
		}
		avgRecall := totalRecall / float64(len(queries)) * 100
		bytesPerVec := M
		memMB := float64(n*bytesPerVec) / 1e6
		float32MB := float64(n*dim*4) / 1e6
		cluster15k := float64(15000*bytesPerVec) / 1e6
		t.Logf("PQ dim=768 M=%d: recall=%.1f%% | %d B/vec | cluster15k=%.2f MB | compression=%.0f×",
			M, avgRecall, bytesPerVec, cluster15k, float32MB/memMB)
	}
}

// TestProtIVF_PQ_Combined — IVF+PQ combined recall на SIFT-128 (реальные данные).
// Цель: recall ≥90% при nprobe=4 + PQ M=16.
func TestProtIVF_PQ_Combined(t *testing.T) {
	if testing.Short() {
		t.Skip("IVF+PQ combined test is slow")
	}
	const K = 10

	vecs, queries, err := loadSIFTRaw("/tmp/sift_raw.bin")
	if err != nil {
		t.Skipf("SIFT raw not found: %v", err)
	}
	n := len(vecs)
	dim := len(vecs[0])

	gt := make([][]int, len(queries))
	for i, q := range queries {
		gt[i] = bruteForceTopKIdx(vecs, q, K)
	}

	// IVF: 16 clusters.
	ivf := trainIVF(vecs, 16)

	// PQ: encode all vectors (M=16, 16 bytes/vector).
	pq := trainPQ(vecs, 16)

	// Combined search: IVF nprobe + PQ-ADC distance, с RERANK на float32.
	// PQ-ADC собирает efFinal кандидатов, потом float32 rerank top-K.
	for _, nprobe := range []int{2, 4, 8} {
		for _, efFinal := range []int{10, 50, 100} {
			ivf.nprobe = nprobe
			totalRecall := 0.0
			for i, q := range queries {
				// Step 1: find nprobe nearest clusters.
				clusters := ivf.searchTopNprobe(q)
				// Step 2: PQ-ADC distances → top-efFinal candidates.
				type vd struct {
					idx  int
					dist float32
				}
				var cands []vd
				for _, c := range clusters {
					for _, vecIdx := range ivf.lists[c] {
						d := pq.adcDistance(q, pq.codes[vecIdx])
						cands = append(cands, vd{vecIdx, d})
					}
				}
				sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
				if len(cands) > efFinal {
					cands = cands[:efFinal]
				}
				// Step 3: RERANK — точный float32 distance для efFinal кандидатов.
				for j := range cands {
					cands[j].dist = EuclideanDistance(q, vecs[cands[j].idx])
				}
				sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
				if len(cands) > K {
					cands = cands[:K]
				}
				found := make([]int, len(cands))
				for j, c := range cands {
					found[j] = c.idx
				}
				totalRecall += measureRecallIdx(found, gt[i])
			}
			avgRecall := totalRecall / float64(len(queries)) * 100
			t.Logf("IVF(16)+PQ(M=16) nprobe=%d efFinal=%d+rerank: recall=%.1f%% | %d B/vec | %.0f× compr",
				nprobe, efFinal, avgRecall, pq.M,
				float64(n*dim*4)/float64(n*pq.M))
		}
	}
}

// TestProt_Memory_Comparison — сравнение memory для разных методов.
func TestProt_Memory_Comparison(t *testing.T) {
	const n = 50000
	const dim = 128

	float32MB := float64(n*dim*4) / 1e6
	sq8MB := float64(n*dim) / 1e6
	pqM16MB := float64(n*16) / 1e6 // M=16 bytes/vector

	fmt.Printf("\n=== MEMORY COMPARISON (n=%d dim=%d) ===\n", n, dim)
	fmt.Printf("  float32:  %.1f MB\n", float32MB)
	fmt.Printf("  SQ8:      %.1f MB (%.1f× compression)\n", sq8MB, float32MB/sq8MB)
	fmt.Printf("  PQ M=16:  %.1f MB (%.1f× compression)\n", pqM16MB, float32MB/pqM16MB)
	fmt.Printf("\n  L3 = 12 MB\n")
	fmt.Printf("  PQ cluster 15k: %.2f MB → %s\n",
		float64(15000*16)/1e6,
		l3Status(float64(15000*16)/1e6))
}

func l3Status(mb float64) string {
	if mb <= 12 {
		return "✓ fits in L3"
	}
	return fmt.Sprintf("❌ %.1f× L3", mb/12)
}
