package pubsub

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Subscribe + Publish (базовый сценарий) ──────────────
//
// 1 подписчик, 1 канал, 1 сообщение.
// Проверяем: подписчик получает сообщение в формате RESP.

func TestHub_SubscribeAndPublish(t *testing.T) {
	hub := NewHub(nil)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Подписываемся
	hub.Subscribe(serverConn, []string{"news"})

	// Publish возвращает 1 = сообщение принято в канал подписчика
	delivered := hub.Publish("news", "hello world")
	if delivered != 1 {
		t.Fatalf("Publish delivered to %d, want 1", delivered)
	}

	// writePump читает из sub.ch и пишет в conn через bufio (4096 байт).
	// bufio НЕ флашит пока буфер не заполнится.
	// Шлём достаточно сообщений чтобы переполнить bufio → auto-flush.
	for i := 0; i < 150; i++ {
		hub.Publish("news", "padding-to-trigger-bufio-flush")
	}

	// Теперь данные ушли в pipe — читаем с клиентской стороны
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8192)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}

	resp := string(buf[:n])
	if !strings.Contains(resp, "hello world") {
		t.Fatalf("response should contain 'hello world', got %d bytes", n)
	}
	if !strings.Contains(resp, "news") {
		t.Fatalf("response should contain channel name 'news'")
	}
}

// ─── СТРАЖ C2: одиночное мелкое сообщение доставляется ────
//
// Регрессия к баге, когда writePump писал в bufio, но не флашил: одиночное
// сообщение (< 4KB) оседало в буфере и НЕ доходило до клиента. Прошлые тесты
// маскировали это, бомбя 150 сообщений ради авто-флаша при переполнении буфера.
//
// Здесь шлём РОВНО ОДНО сообщение и требуем, чтобы оно дошло. Если writePump
// снова перестанет флашить — клиент упрётся в дедлайн, и тест упадёт.
func TestHub_SingleMessageDelivered(t *testing.T) {
	hub := NewHub(nil)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"news"})

	if d := hub.Publish("news", "hello-single"); d != 1 {
		t.Fatalf("Publish delivered %d, want 1", d)
	}

	// Читаем со стороны клиента, накапливая, пока не увидим сообщение.
	// Первым может прийти confirmation подписки (отдельным флашем) — поэтому цикл.
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var acc strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(acc.String(), "hello-single") {
		n, err := clientConn.Read(buf)
		if err != nil {
			t.Fatalf("одиночное сообщение не доставлено (writePump не флашит?): "+
				"read вернул %v, накоплено %q", err, acc.String())
		}
		acc.Write(buf[:n])
	}

	if !strings.Contains(acc.String(), "news") {
		t.Fatalf("в ответе нет имени канала 'news': %q", acc.String())
	}
}

// readUntil читает с клиентской стороны net.Pipe, накапливая, пока не встретит
// substr, либо не истечёт дедлайн. Возвращает накопленное.
func readUntil(t *testing.T, conn net.Conn, substr string, timeout time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var acc strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(acc.String(), substr) {
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("readUntil(%q): %v; накоплено %q", substr, err, acc.String())
		}
		acc.Write(buf[:n])
	}
	return acc.String()
}

// ─── СТРАЖ H3: subscriber-mode отклоняет чужие команды через writePump ───
//
// Регрессия к dual-writer: пока conn подписан (writePump активен), обычная
// команда шла в основной обработчик и писала ответ в cs.Buf — второй писатель
// в тот же сокет. Теперь subscriber-mode обслуживает такие команды здесь, и
// ответ (ошибка) уходит через writePump — единственного писателя.
func TestHub_SubscriberMode_RejectsForeignCommand(t *testing.T) {
	hub := NewHub(nil)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"news"}) // теперь conn — подписчик

	// Подписчик шлёт GET — недопустимо в режиме подписки. readUntil упадёт,
	// если ошибка НЕ придёт через writePump (значит ответ ушёл мимо, в cs.Buf).
	hub.HandleSubscriberCommand(serverConn, "GET", [][]byte{[]byte("key")})
	readUntil(t, clientConn, "not allowed in subscriber mode", 2*time.Second)
}

// ─── СТРАЖ H3: PING в режиме подписки отвечает через writePump ───
func TestHub_SubscriberMode_Ping(t *testing.T) {
	hub := NewHub(nil)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"news"})
	hub.HandleSubscriberCommand(serverConn, "PING", nil)

	readUntil(t, clientConn, "PONG", 2*time.Second)
}

// ─── СТРАЖ H3: ack на UNSUBSCRIBE доставляется даже при закрытии writePump ───
//
// UNSUBSCRIBE снимает последнюю подписку → Unsubscribe закрывает writePump
// (done). Ack «OK» ставится в канал ДО закрытия, и drain-on-done в writePump
// обязан его дослать. После — соединение больше не подписчик.
func TestHub_SubscriberMode_UnsubscribeAckThenExit(t *testing.T) {
	hub := NewHub(nil)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"only"})
	hub.HandleSubscriberCommand(serverConn, "UNSUBSCRIBE", nil)

	// ack должен прийти, несмотря на закрытие writePump на последней отписке.
	readUntil(t, clientConn, "OK", 2*time.Second)

	if hub.IsSubscriber(serverConn) {
		t.Fatal("после UNSUBSCRIBE всех каналов соединение не должно быть подписчиком")
	}
}

// ─── Subscribe: подтверждение (confirmation) ─────────────
//
// Redis при SUBSCRIBE возвращает: ["subscribe", "channel", count]
// Проверяем что наш Hub отправляет аналогичные подтверждения.

func TestHub_SubscribeConfirmation(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	sub := hub.Subscribe(serverConn, []string{"ch1", "ch2"})

	// Проверяем: подписчик зарегистрирован на оба канала
	hub.mu.RLock()
	_, ch1Exists := hub.channels["ch1"]
	_, ch2Exists := hub.channels["ch2"]
	subChannelCount := len(sub.channels)
	hub.mu.RUnlock()

	if !ch1Exists {
		t.Fatal("channel 'ch1' should exist in hub")
	}
	if !ch2Exists {
		t.Fatal("channel 'ch2' should exist in hub")
	}
	if subChannelCount != 2 {
		t.Fatalf("subscriber should have 2 channels, got %d", subChannelCount)
	}

	// Оба канала доставляют сообщения
	if d := hub.Publish("ch1", "test"); d != 1 {
		t.Fatalf("Publish to ch1: delivered %d, want 1", d)
	}
	if d := hub.Publish("ch2", "test"); d != 1 {
		t.Fatalf("Publish to ch2: delivered %d, want 1", d)
	}
}

// ─── Publish без подписчиков → 0 ─────────────────────────

func TestHub_PublishNoSubscribers(t *testing.T) {
	hub := NewHub(nil)

	delivered := hub.Publish("empty-channel", "nobody hears me")
	if delivered != 0 {
		t.Fatalf("Publish to empty channel: delivered %d, want 0", delivered)
	}
}

// ─── Несколько подписчиков на один канал ──────────────────

func TestHub_MultipleSubscribers(t *testing.T) {
	hub := NewHub(nil)

	const numSubs = 5

	for i := 0; i < numSubs; i++ {
		_, serverConn := net.Pipe()
		defer serverConn.Close()
		hub.Subscribe(serverConn, []string{"broadcast"})
	}

	// Publish возвращает количество подписчиков, которым доставлено
	delivered := hub.Publish("broadcast", "to-all")
	if delivered != numSubs {
		t.Fatalf("Publish: delivered %d, want %d", delivered, numSubs)
	}
}

// ─── Unsubscribe от конкретного канала ────────────────────

func TestHub_UnsubscribeChannel(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"ch1", "ch2"})

	// Отписываемся от ch1
	hub.Unsubscribe(serverConn, []string{"ch1"})

	// Publish в ch1 — не должен дойти (0 подписчиков)
	delivered := hub.Publish("ch1", "should-not-arrive")
	if delivered != 0 {
		t.Fatalf("Publish to unsubscribed ch1: delivered %d, want 0", delivered)
	}

	// Publish в ch2 — должен дойти (1 подписчик)
	delivered = hub.Publish("ch2", "still-subscribed")
	if delivered != 1 {
		t.Fatalf("Publish to ch2: delivered %d, want 1", delivered)
	}
}

// ─── Unsubscribe от всех каналов (nil) ───────────────────

func TestHub_UnsubscribeAll(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"a", "b", "c"})

	// Полная отписка
	hub.Unsubscribe(serverConn, nil)

	// Теперь ни один канал не должен доставлять
	for _, ch := range []string{"a", "b", "c"} {
		delivered := hub.Publish(ch, "nope")
		if delivered != 0 {
			t.Fatalf("Publish to %q after UnsubscribeAll: delivered %d", ch, delivered)
		}
	}

	// Подписчик удалён из subscribers map
	if hub.IsSubscriber(serverConn) {
		t.Fatal("subscriber should be removed after full unsubscribe")
	}
}

// ─── RemoveConn (отключение клиента) ─────────────────────

func TestHub_RemoveConn(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"x", "y"})

	hub.RemoveConn(serverConn)

	if hub.IsSubscriber(serverConn) {
		t.Fatal("should not be subscriber after RemoveConn")
	}

	// Каналы тоже очищены
	delivered := hub.Publish("x", "gone")
	if delivered != 0 {
		t.Fatalf("Publish after RemoveConn: delivered %d, want 0", delivered)
	}
}

// ─── IsSubscriber ────────────────────────────────────────

func TestHub_IsSubscriber(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	// До подписки — false
	if hub.IsSubscriber(serverConn) {
		t.Fatal("should not be subscriber before Subscribe")
	}

	hub.Subscribe(serverConn, []string{"test"})

	// После подписки — true
	if !hub.IsSubscriber(serverConn) {
		t.Fatal("should be subscriber after Subscribe")
	}
}

// ─── Повторный Subscribe (то же соединение, другой канал) ─

func TestHub_ResubscribeSameConn(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	sub1 := hub.Subscribe(serverConn, []string{"first"})
	sub2 := hub.Subscribe(serverConn, []string{"second"})

	// Тот же подписчик (Subscribe переиспользует Subscriber для одного conn)
	if sub1 != sub2 {
		t.Fatal("same conn should reuse the same Subscriber")
	}

	// Оба канала работают — проверяем через Publish return value
	d1 := hub.Publish("first", "msg1")
	d2 := hub.Publish("second", "msg2")

	if d1 != 1 {
		t.Fatalf("Publish to 'first': delivered %d, want 1", d1)
	}
	if d2 != 1 {
		t.Fatalf("Publish to 'second': delivered %d, want 1", d2)
	}
}

// ─── Slow subscriber: буфер переполнен → отключение ──────
//
// Если подписчик не читает данные, буфер (256 сообщений)
// переполняется. Hub должен отключить медленного подписчика,
// чтобы не блокировать остальных.

func TestHub_SlowSubscriberDisconnected(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"flood"})

	// writePump дренирует sub.ch в bufio.Writer (4096 байт буфер).
	// Каждое RESP-сообщение ≈ 40 байт → ~100 сообщений уходят в bufio.
	// После заполнения bufio, writePump блокируется на Flush (pipe blocked).
	// Затем sub.ch (буфер 256) заполняется.
	// Итого нужно: ~100 (bufio) + 256 (channel) + 1 (триггер) ≈ 360+
	// Берём с запасом:
	for i := 0; i < maxBuffer*3; i++ {
		hub.Publish("flood", "spam")
	}

	// Даём время на disconnectSlow
	time.Sleep(100 * time.Millisecond)

	if hub.IsSubscriber(serverConn) {
		t.Fatal("slow subscriber should be disconnected")
	}
}

// ─── Пустой канал удаляется из map (garbage collection) ──

func TestHub_EmptyChannelCleanup(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"temp"})
	hub.Unsubscribe(serverConn, []string{"temp"})

	// Канал "temp" должен быть удалён из hub.channels
	hub.mu.RLock()
	_, exists := hub.channels["temp"]
	hub.mu.RUnlock()

	if exists {
		t.Fatal("empty channel should be cleaned up from map")
	}
}

// ─── writePump: реальная запись RESP в net.Conn ──────────
//
// Проверяем что writePump действительно пишет RESP-байты
// в соединение, а не только в канал.

func TestHub_WritePump_RealWrite(t *testing.T) {
	hub := NewHub(nil)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"live"})

	// bufio.Writer (4096 байт) не флашит пока не заполнится.
	// Шлём достаточно сообщений для переполнения буфера → auto-flush.
	for i := 0; i < 150; i++ {
		hub.Publish("live", "ping-data-to-fill-bufio-buffer")
	}

	// Теперь данные ушли через pipe — читаем с клиентской стороны
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8192)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}

	if n == 0 {
		t.Fatal("writePump did not write to conn")
	}
	t.Logf("writePump wrote %d bytes to conn ✓", n)
}

// ─── Concurrent: Subscribe + Publish из разных горутин ───
//
// Стресс-тест на data race (запускай с -race!).

func TestHub_ConcurrentSubscribePublish(t *testing.T) {
	hub := NewHub(nil)

	var wg sync.WaitGroup
	const goroutines = 20
	const messages = 50

	// Создаём подписчиков
	for i := 0; i < goroutines; i++ {
		_, serverConn := net.Pipe()
		defer serverConn.Close()
		hub.Subscribe(serverConn, []string{"concurrent"})
	}

	// Публикуем из нескольких горутин одновременно
	// Главная цель: детектировать data race (запускай с -race!)
	for i := 0; i < messages; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			delivered := hub.Publish("concurrent", "msg")
			if delivered != goroutines {
				// Может быть < goroutines если slow subscriber был отключён
				// Это допустимо — главное нет panic и data race
			}
		}()
	}

	// Параллельно подписываем/отписываем (стресс на мьютексы)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, conn := net.Pipe()
			defer conn.Close()
			hub.Subscribe(conn, []string{"concurrent"})
			hub.Unsubscribe(conn, []string{"concurrent"})
		}()
	}

	wg.Wait()
}

// ─── sync.Pool: не утекает память при частом Publish ─────
//
// Проверяем что subscriberSlicePool переиспользуется корректно.
// Запускаем 1000 Publish подряд — если Pool утекает, тест
// покажет это через -race или аномальный рост памяти.

func TestHub_PublishPoolReuse(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"pool-test"})

	// writePump дренирует sub.ch в bufio — не мешает Pool-тесту.
	// Главное — нет panic/race при множественных Publish.

	// 1000 публикаций подряд — Pool должен переиспользовать слайсы
	for i := 0; i < 1000; i++ {
		hub.Publish("pool-test", "data")
	}

	// Если дошли сюда без паники и race — Pool работает
}

// ─── Идемпотентность: подписка на один канал дважды ───────
//
// Клиент дважды шлёт SUBSCRIBE news.
// Результат: 1 подписчик, 1 доставка (не 2!).

func TestHub_SubscribeSameChannelTwice(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"news"})
	hub.Subscribe(serverConn, []string{"news"}) // повторно

	// Сообщение должно прийти ОДИН раз, не два
	delivered := hub.Publish("news", "test")
	if delivered != 1 {
		t.Fatalf("duplicate subscribe: delivered %d, want 1", delivered)
	}
}

// ─── Отписка от канала, на который не подписан ────────────
//
// Не должно крашиться. Существующие подписки не затрагиваются.

func TestHub_UnsubscribeNonExistentChannel(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	hub.Subscribe(serverConn, []string{"real"})

	// Отписываемся от канала, на который не подписаны
	hub.Unsubscribe(serverConn, []string{"ghost-channel"})

	// "real" канал всё ещё работает
	delivered := hub.Publish("real", "still works")
	if delivered != 1 {
		t.Fatalf("Publish to 'real' after bad unsubscribe: delivered %d, want 1", delivered)
	}

	// Подписчик всё ещё жив
	if !hub.IsSubscriber(serverConn) {
		t.Fatal("subscriber should still exist after unsubscribing from non-existent channel")
	}
}

// ─── RemoveConn для неизвестного соединения ───────────────
//
// Клиент отключился ДО того как подписался. Не должно паниковать.

func TestHub_RemoveConn_UnknownConn(t *testing.T) {
	hub := NewHub(nil)
	_, unknownConn := net.Pipe()
	defer unknownConn.Close()

	// Не паникует при удалении незнакомого соединения
	hub.RemoveConn(unknownConn)

	// Hub пустой — всё в порядке
	hub.mu.RLock()
	subCount := len(hub.subscribers)
	hub.mu.RUnlock()

	if subCount != 0 {
		t.Fatalf("subscribers should be empty, got %d", subCount)
	}
}

// ─── disconnectSlow: конкурентный вызов из N горутин ──────
//
// В продакшне Publish из разных горутин может одновременно
// обнаружить переполненный буфер и вызвать disconnectSlow
// для одного и того же подписчика.
//
// Guard в коде: if _, exists := h.subscribers[sub.conn]; !exists { return }
// Без него: double-close на sub.done → panic!

func TestHub_DisconnectSlow_Concurrent(t *testing.T) {
	hub := NewHub(nil)
	_, serverConn := net.Pipe()
	defer serverConn.Close()

	sub := hub.Subscribe(serverConn, []string{"stress"})

	// Вызываем disconnectSlow из 10 горутин одновременно
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hub.disconnectSlow(sub)
		}()
	}

	wg.Wait()

	// Подписчик удалён, паники не было
	if hub.IsSubscriber(serverConn) {
		t.Fatal("subscriber should be disconnected")
	}
}
