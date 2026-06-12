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

func newTestHub() *Hub {
	store := tcmalloc.NewTCMallocStore(4) // 4 workers для тестов
	index := vector.NewVectorStoreCosine(store)
	return NewHub(index)
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
	hub := newTestHub()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Подписчик интересуется "ML" вектором
	mlVec := []float32{1, 0, 0, 0}
	_, err := hub.SemanticSubscribe(serverConn, mlVec, 0.5)
	if err != nil {
		t.Fatalf("SemanticSubscribe: %v", err)
	}

	// Публикация похожего вектора (близко к ML)
	queryVec := []float32{0.95, 0.1, 0.05, 0.05}
	delivered := hub.SemanticPublish(queryVec, "GPT-5 released!")
	if delivered != 1 {
		t.Fatalf("SemanticPublish similar: delivered %d, want 1", delivered)
	}

	// Ждём и читаем данные (bufio flush через множественные отправки)
	for i := 0; i < 150; i++ {
		hub.SemanticPublish(queryVec, "padding")
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
	hub := newTestHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	// Подписчик интересуется [1,0,0,0] с жёстким threshold
	mlVec := []float32{1, 0, 0, 0}
	_, err := hub.SemanticSubscribe(serverConn, mlVec, 0.3)
	if err != nil {
		t.Fatalf("SemanticSubscribe: %v", err)
	}

	// Публикация ортогонального вектора (distance ≈ 1.0 >> threshold 0.3)
	orthogonal := []float32{0, 0, 0, 1}
	delivered := hub.SemanticPublish(orthogonal, "cooking recipe")
	if delivered != 0 {
		t.Fatalf("SemanticPublish orthogonal: delivered %d, want 0", delivered)
	}

	// Публикация похожего вектора (distance ≈ 0.004 < threshold 0.3)
	similar := []float32{0.95, 0.1, 0.05, 0.05}
	delivered = hub.SemanticPublish(similar, "ML news")
	if delivered != 1 {
		t.Fatalf("SemanticPublish similar: delivered %d, want 1", delivered)
	}
}

func TestSemanticHub_MultipleSubscribers(t *testing.T) {
	hub := newTestHub()

	// Подписчик 1: ML (порог 0.5)
	_, mlConn := net.Pipe()
	defer mlConn.Close()
	hub.SemanticSubscribe(mlConn, []float32{1, 0, 0, 0}, 0.5)

	// Подписчик 2: Кулинария (порог 0.5)
	_, cookConn := net.Pipe()
	defer cookConn.Close()
	hub.SemanticSubscribe(cookConn, []float32{0, 0, 0, 1}, 0.5)

	// Подписчик 3: Широкие интересы (порог 2.0 — всё)
	_, allConn := net.Pipe()
	defer allConn.Close()
	hub.SemanticSubscribe(allConn, []float32{0.5, 0.5, 0.5, 0.5}, 2.0)

	// Публикация ML-контента
	delivered := hub.SemanticPublish([]float32{0.9, 0.1, 0.05, 0.05}, "AI breakthrough")
	if delivered != 2 {
		t.Fatalf("SemanticPublish ML content: delivered %d, want 2", delivered)
	}

	// Публикация кулинарного контента
	delivered = hub.SemanticPublish([]float32{0.05, 0.05, 0.1, 0.9}, "new recipe")
	if delivered != 2 {
		t.Fatalf("SemanticPublish cooking content: delivered %d, want 2", delivered)
	}
}

func TestSemanticHub_PublishNoSubscribers(t *testing.T) {
	hub := newTestHub()

	delivered := hub.SemanticPublish([]float32{1, 0, 0, 0}, "nobody hears")
	if delivered != 0 {
		t.Fatalf("SemanticPublish to empty hub: delivered %d, want 0", delivered)
	}
}

func TestSemanticHub_Unsubscribe(t *testing.T) {
	hub := newTestHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.SemanticSubscribe(serverConn, []float32{1, 0, 0, 0}, 0.5)

	if !hub.IsSubscriber(serverConn) {
		t.Fatal("should be subscriber after SemanticSubscribe")
	}

	ok := hub.SemanticUnsubscribe(serverConn)
	if !ok {
		t.Fatal("SemanticUnsubscribe should return true")
	}

	if hub.IsSubscriber(serverConn) {
		t.Fatal("should not be subscriber after SemanticUnsubscribe")
	}

	// Публикация после отписки
	delivered := hub.SemanticPublish([]float32{1, 0, 0, 0}, "should not arrive")
	if delivered != 0 {
		t.Fatalf("SemanticPublish after unsubscribe: delivered %d, want 0", delivered)
	}
}

func TestSemanticHub_UnsubscribeNonExistent(t *testing.T) {
	hub := newTestHub()
	_, conn := net.Pipe()
	defer conn.Close()

	ok := hub.SemanticUnsubscribe(conn)
	if ok {
		t.Fatal("SemanticUnsubscribe non-existent should return false")
	}
}

func TestSemanticHub_RemoveConn(t *testing.T) {
	hub := newTestHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.SemanticSubscribe(serverConn, []float32{1, 0, 0, 0}, 0.5)
	hub.RemoveConn(serverConn)

	if hub.IsSubscriber(serverConn) {
		t.Fatal("should not be subscriber after RemoveConn")
	}

	if hub.SemanticSubscriberCount() != 0 {
		t.Fatalf("SemanticSubscriberCount: got %d, want 0", hub.SemanticSubscriberCount())
	}
}

func TestSemanticHub_ResubscribeSameConn(t *testing.T) {
	hub := newTestHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	// Первая подписка: ML
	hub.SemanticSubscribe(serverConn, []float32{1, 0, 0, 0}, 0.5)

	// Вторая подписка: кулинария (заменяет ML)
	hub.SemanticSubscribe(serverConn, []float32{0, 0, 0, 1}, 0.5)

	if hub.SemanticSubscriberCount() != 1 {
		t.Fatalf("SemanticSubscriberCount: got %d, want 1", hub.SemanticSubscriberCount())
	}

	// ML-контент НЕ должен доставляться (подписка заменена)
	delivered := hub.SemanticPublish([]float32{0.95, 0.1, 0, 0}, "ML news")
	if delivered != 0 {
		t.Fatalf("SemanticPublish ML after resubscribe: delivered %d, want 0", delivered)
	}

	// Кулинарный контент ДОЛЖЕН доставляться
	delivered = hub.SemanticPublish([]float32{0, 0, 0.1, 0.95}, "new recipe")
	if delivered != 1 {
		t.Fatalf("SemanticPublish cooking after resubscribe: delivered %d, want 1", delivered)
	}
}

func TestSemanticHub_SubscriberCount(t *testing.T) {
	hub := newTestHub()

	if hub.SemanticSubscriberCount() != 0 {
		t.Fatalf("initial count: got %d, want 0", hub.SemanticSubscriberCount())
	}

	conns := make([]net.Conn, 5)
	for i := 0; i < 5; i++ {
		_, serverConn := net.Pipe()
		defer serverConn.Close()
		conns[i] = serverConn
		vec := make([]float32, 4)
		vec[i%4] = 1
		hub.SemanticSubscribe(serverConn, vec, 0.5)
	}

	if hub.SemanticSubscriberCount() != 5 {
		t.Fatalf("after 5 subscribes: got %d, want 5", hub.SemanticSubscriberCount())
	}

	hub.SemanticUnsubscribe(conns[0])
	hub.SemanticUnsubscribe(conns[1])

	if hub.SemanticSubscriberCount() != 3 {
		t.Fatalf("after 2 unsubscribes: got %d, want 3", hub.SemanticSubscriberCount())
	}
}

func TestSemanticHub_IsSubscriber(t *testing.T) {
	hub := newTestHub()
	_, conn := net.Pipe()
	defer conn.Close()

	if hub.IsSubscriber(conn) {
		t.Fatal("should not be subscriber before SemanticSubscribe")
	}

	hub.SemanticSubscribe(conn, []float32{1, 0, 0, 0}, 0.5)

	if !hub.IsSubscriber(conn) {
		t.Fatal("should be subscriber after SemanticSubscribe")
	}

	hub.SemanticUnsubscribe(conn)

	if hub.IsSubscriber(conn) {
		t.Fatal("should not be subscriber after SemanticUnsubscribe")
	}
}

func TestSemanticHub_SlowSubscriberDisconnected(t *testing.T) {
	hub := newTestHub()
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.SemanticSubscribe(serverConn, []float32{1, 0, 0, 0}, 2.0) // threshold=2.0, всё получает

	// Заливаем канал
	similar := []float32{0.9, 0.1, 0.05, 0.05}
	for i := 0; i < maxBuffer*3; i++ {
		hub.SemanticPublish(similar, "spam")
	}

	time.Sleep(100 * time.Millisecond)

	if hub.IsSubscriber(serverConn) {
		t.Fatal("slow subscriber should be disconnected")
	}
}

func TestSemanticHub_ConcurrentPublish(t *testing.T) {
	hub := newTestHub()

	const numSubs = 10
	const numPublishers = 20
	const numMessages = 50

	// Создаём подписчиков
	for i := 0; i < numSubs; i++ {
		_, conn := net.Pipe()
		defer conn.Close()
		vec := make([]float32, 4)
		vec[i%4] = 1
		hub.SemanticSubscribe(conn, vec, 2.0)
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
				hub.SemanticPublish(vec, "concurrent msg")
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
			hub.SemanticSubscribe(conn, []float32{0.5, 0.5, 0.5, 0.5}, 1.0)
			hub.SemanticUnsubscribe(conn)
		}()
	}

	wg.Wait()
}

func TestSemanticHub_HighDimensional(t *testing.T) {
	hub := newTestHub()

	rng := rand.New(rand.NewSource(42))
	dim := 128

	// Создаём 2 подписчика с ортогональными интересами
	v1 := randomVec(dim, rng)
	_, conn1 := net.Pipe()
	defer conn1.Close()
	hub.SemanticSubscribe(conn1, v1, 0.3)

	v2 := randomVec(dim, rng)
	_, conn2 := net.Pipe()
	defer conn2.Close()
	hub.SemanticSubscribe(conn2, v2, 0.3)

	// Публикация вектора, идентичного v1
	delivered := hub.SemanticPublish(v1, "matches v1")
	if delivered < 1 {
		t.Fatalf("SemanticPublish identical to v1: delivered %d, want ≥1", delivered)
	}
}

func TestSemanticHub_ZeroThreshold(t *testing.T) {
	hub := newTestHub()
	_, conn := net.Pipe()
	defer conn.Close()

	vec := []float32{1, 0, 0, 0}
	hub.SemanticSubscribe(conn, vec, 0.0)

	// Даже слегка другой вектор не должен проходить
	similar := []float32{0.99, 0.01, 0, 0}
	delivered := hub.SemanticPublish(similar, "almost identical")
	if delivered != 0 {
		t.Fatalf("SemanticPublish with zero threshold: delivered %d, want 0", delivered)
	}

	// Идентичный вектор — distance = 0 → delivered
	delivered = hub.SemanticPublish(vec, "exact match")
	if delivered != 1 {
		t.Fatalf("SemanticPublish exact match: delivered %d, want 1", delivered)
	}
}

// ─── Mixed Subscriptions (Classic + Semantic on same conn) ───

func TestHub_MixedSubscriptions(t *testing.T) {
	hub := newTestHub()
	_, conn := net.Pipe()
	defer conn.Close()

	// Подписка на classic канал
	hub.Subscribe(conn, []string{"news"})

	// Подписка на semantic (тот же conn!)
	hub.SemanticSubscribe(conn, []float32{1, 0, 0, 0}, 0.5)

	if !hub.IsSubscriber(conn) {
		t.Fatal("should be subscriber")
	}

	// Classic publish → доставляется
	delivered := hub.Publish("news", "breaking news")
	if delivered != 1 {
		t.Fatalf("Classic publish: delivered %d, want 1", delivered)
	}

	// Semantic publish → тоже доставляется тому же подписчику
	delivered = hub.SemanticPublish([]float32{0.95, 0.05, 0, 0}, "ML news")
	if delivered != 1 {
		t.Fatalf("Semantic publish: delivered %d, want 1", delivered)
	}

	// Отписка от classic — semantic остаётся
	hub.Unsubscribe(conn, nil) // отписка от всех classic каналов
	if !hub.IsSubscriber(conn) {
		t.Fatal("should still be subscriber (semantic active)")
	}

	// Semantic publish всё ещё работает
	delivered = hub.SemanticPublish([]float32{0.95, 0.05, 0, 0}, "still works")
	if delivered != 1 {
		t.Fatalf("Semantic after classic unsub: delivered %d, want 1", delivered)
	}

	// Отписка от semantic — subscriber удаляется
	hub.SemanticUnsubscribe(conn)
	if hub.IsSubscriber(conn) {
		t.Fatal("should not be subscriber after both unsubscribed")
	}
}

func TestHub_RemoveConnCleansBoth(t *testing.T) {
	hub := newTestHub()
	_, conn := net.Pipe()
	defer conn.Close()

	hub.Subscribe(conn, []string{"alerts"})
	hub.SemanticSubscribe(conn, []float32{1, 0, 0, 0}, 0.5)

	// RemoveConn должен удалить ОБА типа подписок
	hub.RemoveConn(conn)

	if hub.IsSubscriber(conn) {
		t.Fatal("should not be subscriber after RemoveConn")
	}
	if hub.SemanticSubscriberCount() != 0 {
		t.Fatalf("semantic subs after RemoveConn: %d, want 0", hub.SemanticSubscriberCount())
	}

	// Ни classic, ни semantic publish не должны доставляться
	if hub.Publish("alerts", "test") != 0 {
		t.Fatal("classic publish should deliver 0 after RemoveConn")
	}
	if hub.SemanticPublish([]float32{1, 0, 0, 0}, "test") != 0 {
		t.Fatal("semantic publish should deliver 0 after RemoveConn")
	}
}

// ─── Benchmarks ──────────────────────────────────────────────

func BenchmarkSemanticHub_Subscribe(b *testing.B) {
	store := tcmalloc.NewTCMallocStore(4)
	index := vector.NewVectorStoreCosine(store)
	hub := NewHub(index)

	vec := normalizeVec([]float32{1, 0, 0, 0})

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, conn := net.Pipe()
		hub.SemanticSubscribe(conn, vec, 0.5)
		hub.SemanticUnsubscribe(conn)
		conn.Close()
	}
}

func benchmarkSemanticPublish(b *testing.B, numSubs int, dim int) {
	store := tcmalloc.NewTCMallocStore(4)
	index := vector.NewVectorStoreCosine(store)
	hub := NewHub(index)

	rng := rand.New(rand.NewSource(42))

	// Создаём подписчиков с случайными векторами
	for i := 0; i < numSubs; i++ {
		_, conn := net.Pipe()
		defer conn.Close()
		vec := randomVec(dim, rng)
		hub.SemanticSubscribe(conn, vec, 0.5)
	}

	queryVec := randomVec(dim, rng)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		hub.SemanticPublish(queryVec, "bench message")
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
	hub := NewHub(index)

	rng := rand.New(rand.NewSource(42))

	for i := 0; i < 100; i++ {
		_, conn := net.Pipe()
		defer conn.Close()
		hub.SemanticSubscribe(conn, randomVec(32, rng), 0.5)
	}

	queryVec := randomVec(32, rng)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			hub.SemanticPublish(queryVec, "concurrent bench")
		}
	})
}
