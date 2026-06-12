package pubsub

import (
	"math"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/vector"
)

// ─── Helpers ─────────────────────────────────────────────────

func newTestSemanticHub() *SemanticHub {
	store := tcmalloc.NewTCMallocStore(4) // 4 workers для тестов
	index := vector.NewVectorStoreCosine(store)
	return NewSemanticHub(index)
}

// normalizeVec нормализует вектор (для предсказуемых distance значений).
func normalizeVec(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	var norm float32
	for _, x := range out {
		norm += x * x
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm > 0 {
		for i := range out {
			out[i] /= norm
		}
	}
	return out
}

// randomVec генерирует случайный нормализованный вектор.
func randomVec(dim int, rng *rand.Rand) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return normalizeVec(v)
}

// ─── Basic Subscribe + Publish ───────────────────────────────

func TestSemanticHub_SubscribeAndPublish(t *testing.T) {
	sh := newTestSemanticHub()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Подписчик интересуется "ML" вектором
	mlVec := []float32{1, 0, 0, 0}
	_, err := sh.Subscribe(serverConn, mlVec, 0.5)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Публикация похожего вектора (близко к ML)
	queryVec := []float32{0.95, 0.1, 0.05, 0.05}
	delivered := sh.Publish(queryVec, "GPT-5 released!")
	if delivered != 1 {
		t.Fatalf("Publish similar: delivered %d, want 1", delivered)
	}

	// Ждём и читаем данные (bufio flush через множественные отправки)
	for i := 0; i < 150; i++ {
		sh.Publish(queryVec, "padding")
	}

	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16384)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}

	resp := string(buf[:n])
	if !strings.Contains(resp, "semantic-message") {
		t.Fatalf("response should contain 'semantic-message', got %d bytes", n)
	}
	if !strings.Contains(resp, "GPT-5 released!") {
		t.Fatalf("response should contain 'GPT-5 released!'")
	}
}

func TestSemanticHub_ThresholdFiltering(t *testing.T) {
	sh := newTestSemanticHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	// Подписчик интересуется [1,0,0,0] с жёстким threshold
	mlVec := []float32{1, 0, 0, 0}
	_, err := sh.Subscribe(serverConn, mlVec, 0.3)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Публикация ортогонального вектора (distance ≈ 1.0 >> threshold 0.3)
	orthogonal := []float32{0, 0, 0, 1}
	delivered := sh.Publish(orthogonal, "cooking recipe")
	if delivered != 0 {
		t.Fatalf("Publish orthogonal: delivered %d, want 0", delivered)
	}

	// Публикация похожего вектора (distance ≈ 0.004 < threshold 0.3)
	similar := []float32{0.95, 0.1, 0.05, 0.05}
	delivered = sh.Publish(similar, "ML news")
	if delivered != 1 {
		t.Fatalf("Publish similar: delivered %d, want 1", delivered)
	}
}

func TestSemanticHub_MultipleSubscribers(t *testing.T) {
	sh := newTestSemanticHub()

	// Подписчик 1: ML (порог 0.5)
	_, mlConn := net.Pipe()
	defer mlConn.Close()
	sh.Subscribe(mlConn, []float32{1, 0, 0, 0}, 0.5)

	// Подписчик 2: Кулинария (порог 0.5)
	_, cookConn := net.Pipe()
	defer cookConn.Close()
	sh.Subscribe(cookConn, []float32{0, 0, 0, 1}, 0.5)

	// Подписчик 3: Широкие интересы (порог 2.0 — всё)
	_, allConn := net.Pipe()
	defer allConn.Close()
	sh.Subscribe(allConn, []float32{0.5, 0.5, 0.5, 0.5}, 2.0)

	// Публикация ML-контента
	delivered := sh.Publish([]float32{0.9, 0.1, 0.05, 0.05}, "AI breakthrough")
	// ML подписчик: distance ≈ 0 ≤ 0.5 → доставлено ✅
	// Cook подписчик: distance ≈ 1.0 > 0.5 → НЕ доставлено ❌
	// All подписчик: distance < 2.0 → доставлено ✅
	if delivered != 2 {
		t.Fatalf("Publish ML content: delivered %d, want 2", delivered)
	}

	// Публикация кулинарного контента
	delivered = sh.Publish([]float32{0.05, 0.05, 0.1, 0.9}, "new recipe")
	// ML: distance ≈ 1.0 > 0.5 → ❌
	// Cook: distance ≈ 0 ≤ 0.5 → ✅
	// All: distance < 2.0 → ✅
	if delivered != 2 {
		t.Fatalf("Publish cooking content: delivered %d, want 2", delivered)
	}
}

func TestSemanticHub_PublishNoSubscribers(t *testing.T) {
	sh := newTestSemanticHub()

	delivered := sh.Publish([]float32{1, 0, 0, 0}, "nobody hears")
	if delivered != 0 {
		t.Fatalf("Publish to empty hub: delivered %d, want 0", delivered)
	}
}

func TestSemanticHub_Unsubscribe(t *testing.T) {
	sh := newTestSemanticHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	sh.Subscribe(serverConn, []float32{1, 0, 0, 0}, 0.5)

	if !sh.IsSubscriber(serverConn) {
		t.Fatal("should be subscriber after Subscribe")
	}

	ok := sh.Unsubscribe(serverConn)
	if !ok {
		t.Fatal("Unsubscribe should return true")
	}

	if sh.IsSubscriber(serverConn) {
		t.Fatal("should not be subscriber after Unsubscribe")
	}

	// Публикация после отписки
	delivered := sh.Publish([]float32{1, 0, 0, 0}, "should not arrive")
	if delivered != 0 {
		t.Fatalf("Publish after unsubscribe: delivered %d, want 0", delivered)
	}
}

func TestSemanticHub_UnsubscribeNonExistent(t *testing.T) {
	sh := newTestSemanticHub()
	_, conn := net.Pipe()
	defer conn.Close()

	ok := sh.Unsubscribe(conn)
	if ok {
		t.Fatal("Unsubscribe non-existent should return false")
	}
}

func TestSemanticHub_RemoveConn(t *testing.T) {
	sh := newTestSemanticHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	sh.Subscribe(serverConn, []float32{1, 0, 0, 0}, 0.5)
	sh.RemoveConn(serverConn)

	if sh.IsSubscriber(serverConn) {
		t.Fatal("should not be subscriber after RemoveConn")
	}

	if sh.SubscriberCount() != 0 {
		t.Fatalf("SubscriberCount: got %d, want 0", sh.SubscriberCount())
	}
}

func TestSemanticHub_ResubscribeSameConn(t *testing.T) {
	sh := newTestSemanticHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	// Первая подписка: ML
	sh.Subscribe(serverConn, []float32{1, 0, 0, 0}, 0.5)

	// Вторая подписка: кулинария (заменяет ML)
	sh.Subscribe(serverConn, []float32{0, 0, 0, 1}, 0.5)

	if sh.SubscriberCount() != 1 {
		t.Fatalf("SubscriberCount: got %d, want 1", sh.SubscriberCount())
	}

	// ML-контент НЕ должен доставляться (подписка заменена)
	delivered := sh.Publish([]float32{0.95, 0.1, 0, 0}, "ML news")
	if delivered != 0 {
		t.Fatalf("Publish ML after resubscribe: delivered %d, want 0", delivered)
	}

	// Кулинарный контент ДОЛЖЕН доставляться
	delivered = sh.Publish([]float32{0, 0, 0.1, 0.95}, "new recipe")
	if delivered != 1 {
		t.Fatalf("Publish cooking after resubscribe: delivered %d, want 1", delivered)
	}
}

func TestSemanticHub_SubscriberCount(t *testing.T) {
	sh := newTestSemanticHub()

	if sh.SubscriberCount() != 0 {
		t.Fatalf("initial count: got %d, want 0", sh.SubscriberCount())
	}

	conns := make([]net.Conn, 5)
	for i := 0; i < 5; i++ {
		_, serverConn := net.Pipe()
		defer serverConn.Close()
		conns[i] = serverConn
		vec := make([]float32, 4)
		vec[i%4] = 1
		sh.Subscribe(serverConn, vec, 0.5)
	}

	if sh.SubscriberCount() != 5 {
		t.Fatalf("after 5 subscribes: got %d, want 5", sh.SubscriberCount())
	}

	sh.Unsubscribe(conns[0])
	sh.Unsubscribe(conns[1])

	if sh.SubscriberCount() != 3 {
		t.Fatalf("after 2 unsubscribes: got %d, want 3", sh.SubscriberCount())
	}
}

func TestSemanticHub_IsSubscriber(t *testing.T) {
	sh := newTestSemanticHub()
	_, conn := net.Pipe()
	defer conn.Close()

	if sh.IsSubscriber(conn) {
		t.Fatal("should not be subscriber before Subscribe")
	}

	sh.Subscribe(conn, []float32{1, 0, 0, 0}, 0.5)

	if !sh.IsSubscriber(conn) {
		t.Fatal("should be subscriber after Subscribe")
	}

	sh.Unsubscribe(conn)

	if sh.IsSubscriber(conn) {
		t.Fatal("should not be subscriber after Unsubscribe")
	}
}

func TestSemanticHub_SlowSubscriberDisconnected(t *testing.T) {
	sh := newTestSemanticHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	sh.Subscribe(serverConn, []float32{1, 0, 0, 0}, 2.0) // threshold=2.0, всё получает

	// Заливаем канал
	similar := []float32{0.9, 0.1, 0.05, 0.05}
	for i := 0; i < maxBuffer*3; i++ {
		sh.Publish(similar, "spam")
	}

	time.Sleep(100 * time.Millisecond)

	if sh.IsSubscriber(serverConn) {
		t.Fatal("slow subscriber should be disconnected")
	}
}

func TestSemanticHub_ConcurrentPublish(t *testing.T) {
	sh := newTestSemanticHub()

	const numSubs = 10
	const numPublishers = 20
	const numMessages = 50

	// Создаём подписчиков
	for i := 0; i < numSubs; i++ {
		_, conn := net.Pipe()
		defer conn.Close()
		vec := make([]float32, 4)
		vec[i%4] = 1
		sh.Subscribe(conn, vec, 2.0)
	}

	// Параллельная публикация
	var wg sync.WaitGroup
	for i := 0; i < numPublishers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			vec := make([]float32, 4)
			vec[id%4] = 1
			for j := 0; j < numMessages; j++ {
				sh.Publish(vec, "concurrent msg")
			}
		}(i)
	}

	// Параллельные subscribe/unsubscribe
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, conn := net.Pipe()
			defer conn.Close()
			sh.Subscribe(conn, []float32{0.5, 0.5, 0.5, 0.5}, 1.0)
			sh.Unsubscribe(conn)
		}()
	}

	wg.Wait()
}

func TestSemanticHub_HighDimensional(t *testing.T) {
	sh := newTestSemanticHub()

	rng := rand.New(rand.NewSource(42))
	dim := 128

	// Создаём 2 подписчика с ортогональными интересами
	v1 := randomVec(dim, rng)
	_, conn1 := net.Pipe()
	defer conn1.Close()
	sh.Subscribe(conn1, v1, 0.3)

	v2 := randomVec(dim, rng)
	_, conn2 := net.Pipe()
	defer conn2.Close()
	sh.Subscribe(conn2, v2, 0.3)

	// Публикация вектора, идентичного v1
	delivered := sh.Publish(v1, "matches v1")
	// v1 should match v1 (distance=0) and likely NOT match v2 (random, distance≈0.5-1.0)
	if delivered < 1 {
		t.Fatalf("Publish identical to v1: delivered %d, want ≥1", delivered)
	}
}

func TestSemanticHub_ZeroThreshold(t *testing.T) {
	sh := newTestSemanticHub()
	_, conn := net.Pipe()
	defer conn.Close()

	vec := []float32{1, 0, 0, 0}
	sh.Subscribe(conn, vec, 0.0)

	// Даже слегка другой вектор не должен проходить
	similar := []float32{0.99, 0.01, 0, 0}
	delivered := sh.Publish(similar, "almost identical")
	// Distance > 0 but threshold = 0 → not delivered
	if delivered != 0 {
		t.Fatalf("Publish with zero threshold: delivered %d, want 0", delivered)
	}

	// Идентичный вектор — distance = 0 → delivered
	delivered = sh.Publish(vec, "exact match")
	if delivered != 1 {
		t.Fatalf("Publish exact match: delivered %d, want 1", delivered)
	}
}

// ─── Benchmarks ──────────────────────────────────────────────

func BenchmarkSemanticHub_Subscribe(b *testing.B) {
	store := tcmalloc.NewTCMallocStore(4)
	index := vector.NewVectorStoreCosine(store)
	sh := NewSemanticHub(index)

	vec := normalizeVec([]float32{1, 0, 0, 0})

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, conn := net.Pipe()
		sh.Subscribe(conn, vec, 0.5)
		sh.Unsubscribe(conn)
		conn.Close()
	}
}

func benchmarkSemanticPublish(b *testing.B, numSubs int, dim int) {
	store := tcmalloc.NewTCMallocStore(4)
	index := vector.NewVectorStoreCosine(store)
	sh := NewSemanticHub(index)

	rng := rand.New(rand.NewSource(42))

	// Создаём подписчиков с случайными векторами
	for i := 0; i < numSubs; i++ {
		_, conn := net.Pipe()
		defer conn.Close()
		vec := randomVec(dim, rng)
		sh.Subscribe(conn, vec, 0.5)
	}

	queryVec := randomVec(dim, rng)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sh.Publish(queryVec, "bench message")
	}
}

func BenchmarkSemanticHub_Publish_10Subs_4D(b *testing.B) {
	benchmarkSemanticPublish(b, 10, 4)
}

func BenchmarkSemanticHub_Publish_100Subs_4D(b *testing.B) {
	benchmarkSemanticPublish(b, 100, 4)
}

func BenchmarkSemanticHub_Publish_1000Subs_4D(b *testing.B) {
	benchmarkSemanticPublish(b, 1000, 4)
}

func BenchmarkSemanticHub_Publish_10Subs_128D(b *testing.B) {
	benchmarkSemanticPublish(b, 10, 128)
}

func BenchmarkSemanticHub_Publish_100Subs_128D(b *testing.B) {
	benchmarkSemanticPublish(b, 100, 128)
}

func BenchmarkSemanticHub_Publish_1000Subs_128D(b *testing.B) {
	benchmarkSemanticPublish(b, 1000, 128)
}

func BenchmarkSemanticHub_Publish_10Subs_768D(b *testing.B) {
	benchmarkSemanticPublish(b, 10, 768)
}

func BenchmarkSemanticHub_Publish_100Subs_768D(b *testing.B) {
	benchmarkSemanticPublish(b, 100, 768)
}

func BenchmarkSemanticHub_Publish_ConcurrentReaders(b *testing.B) {
	store := tcmalloc.NewTCMallocStore(4)
	index := vector.NewVectorStoreCosine(store)
	sh := NewSemanticHub(index)

	rng := rand.New(rand.NewSource(42))

	for i := 0; i < 100; i++ {
		_, conn := net.Pipe()
		defer conn.Close()
		sh.Subscribe(conn, randomVec(32, rng), 0.5)
	}

	queryVec := randomVec(32, rng)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sh.Publish(queryVec, "concurrent bench")
		}
	})
}
