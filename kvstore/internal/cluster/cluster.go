package cluster

import (
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"kvstore/kvstore/internal/protocol"
)

// ============================================================
// Cluster — главная структура кластера.
//
// Живёт в main.go, передаётся в executeCommand.
// Если cluster == nil — работаем в одиночном режиме (как сейчас).
// Если cluster != nil — проверяем слоты перед каждой командой.
//
// Пример инициализации (3 ноды):
//
//	// На сервере в Алматы (порт 6380):
//	c := cluster.New("10.0.1.1:6380", 6381)
//	c.State.Self.AssignSlots(0, 5460)
//	c.State.RebuildSlotTable()
//
//	// Потом через CLUSTER MEET добавятся остальные ноды
//
// ============================================================
type Cluster struct {
	State          *ClusterState
	gossipListener net.Listener   // TCP listener для gossip
	stopCh         chan struct{}  // Сигнал остановки всех горутин
	wg             sync.WaitGroup // Ожидание завершения горутин
}

// New создаёт кластер с текущей нодой.
//
// addr — адрес для клиентов ("10.0.1.1:6380")
// gossipPort — порт для gossip (6381)
//
// При создании генерируется случайный ID ноды (8 hex-символов).
// В реальном Redis ID = 40 hex, но для наших целей 8 хватит.
func New(addr string, gossipPort int) *Cluster {
	id := generateNodeID()
	self := NewNode(id, addr, gossipPort)
	return &Cluster{
		State:  NewClusterState(self),
		stopCh: make(chan struct{}),
	}
}

// generateNodeID создаёт случайный ID для ноды.
//
// Пример: "a1b2c3d4"
//
// Зачем случайный, а не hostname?
//
//	Hostname может совпасть (два сервера "server-1").
//	Случайный 8-байтовый ID — вероятность коллизии 1 на 4 миллиарда.
func generateNodeID() string {
	b := make([]byte, 4) // 4 байта = 8 hex-символов
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// ============================================================
// CheckKey — главная функция маршрутизации.
//
// Вызывается ПЕРЕД выполнением каждой команды (SET, GET, DEL, ...).
// Проверяет: «ключ попадает в мой слот?»
//
// Возвращает:
//
//	nil        → слот мой, можно выполнять команду
//	Value      → -MOVED ответ, команду НЕ выполняем, возвращаем клиенту
//
// Пример на нашем сценарии:
//
//	Мы — Node-A (Алматы), владеем слотами 0-5460.
//	Клиент: SET user:1001 "Николай"
//
//	slot := KeySlot("user:1001")  = 9425
//	cs.IsMySlot(9425)             = false (слот Node-B)
//	owner := cs.LookupSlot(9425)  = node_астана ("10.0.2.1:6380")
//
//	return "-MOVED 9425 10.0.2.1:6380"
//	Клиент получает ошибку и переподключается к Node-B.
//
// ============================================================
func (c *Cluster) CheckKey(key string) *protocol.Value {
	slot := KeySlot(key)

	// Слот мой → nil (выполняем команду)
	if c.State.IsMySlot(slot) {
		return nil
	}

	// Слот не мой → ищем владельца
	owner := c.State.LookupSlot(slot)
	if owner == nil {
		// Слот никому не назначен → ошибка
		errMsg := fmt.Sprintf("CLUSTERDOWN Hash slot %d is not served", slot)
		return &protocol.Value{Typ: '-', Str: errMsg}
	}

	// Формируем MOVED ответ
	// Формат: -MOVED <slot> <addr>
	// Пример: -MOVED 9425 10.0.2.1:6380
	moved := fmt.Sprintf("MOVED %d %s", slot, owner.Addr)
	return &protocol.Value{Typ: '-', Str: moved}
}

// ============================================================
// HandleClusterCommand обрабатывает команды CLUSTER *.
//
// Поддерживаемые подкоманды:
//
//	CLUSTER INFO    — общая информация о кластере
//	CLUSTER NODES   — список всех нод
//	CLUSTER SLOTS   — таблица слот→нода
//	CLUSTER MEET    — знакомство с новой нодой
//	CLUSTER MYID    — ID текущей ноды
//
// Это META-команды — они не работают с данными,
// они работают с КЛАСТЕРОМ (топология, состояние).
// ============================================================
func (c *Cluster) HandleClusterCommand(args []protocol.Value) protocol.Value {
	if len(args) == 0 {
		return protocol.Value{
			Typ: '-',
			Str: "ERR wrong number of arguments for 'cluster' command",
		}
	}

	subcommand := strings.ToUpper(args[0].Str)

	switch subcommand {

	case "MYID":
		// CLUSTER MYID → возвращает ID текущей ноды
		// Пример ответа: "a1b2c3d4"
		return protocol.Value{Typ: '$', Str: c.State.Self.ID}

	case "INFO":
		return c.clusterInfo()

	case "NODES":
		return c.clusterNodes()

	case "SLOTS":
		return c.clusterSlots()

	case "MEET":
		return c.clusterMeet(args[1:])

	default:
		return protocol.Value{
			Typ: '-',
			Str: fmt.Sprintf("ERR unknown subcommand '%s'", subcommand),
		}
	}
}

// ============================================================
// clusterInfo — CLUSTER INFO
//
// Возвращает общую информацию текстом (как в Redis).
// Клиент парсит это как bulk string.
//
// Пример вывода:
//
//	cluster_state:ok
//	cluster_slots_assigned:16384
//	cluster_known_nodes:3
//	cluster_size:3
//
// ============================================================
func (c *Cluster) clusterInfo() protocol.Value {
	c.State.mu.RLock()
	defer c.State.mu.RUnlock()

	// Считаем назначенные слоты
	assigned := 0
	for _, node := range c.State.SlotTable {
		if node != nil {
			assigned++
		}
	}

	// Состояние кластера: ok если все 16384 слота назначены
	state := "ok"
	if assigned < TotalSlots {
		state = "fail"
	}

	// Считаем ноды со слотами (cluster_size)
	nodesWithSlots := 0
	for _, node := range c.State.Nodes {
		if node.SlotCount() > 0 {
			nodesWithSlots++
		}
	}

	info := fmt.Sprintf(
		"cluster_state:%s\r\n"+
			"cluster_slots_assigned:%d\r\n"+
			"cluster_slots_ok:%d\r\n"+
			"cluster_known_nodes:%d\r\n"+
			"cluster_size:%d\r\n",
		state,
		assigned,
		assigned,
		len(c.State.Nodes),
		nodesWithSlots,
	)

	return protocol.Value{Typ: '$', Str: info}
}

// ============================================================
// clusterNodes — CLUSTER NODES
//
// Возвращает список всех нод в текстовом формате.
// Каждая нода — одна строка.
//
// Формат строки:
//
//	<id> <addr> <flags> <slots>
//
// Пример:
//
//	a1b2c3d4 10.0.1.1:6380 myself,online 0-5460
//	e5f6g7h8 10.0.2.1:6380 online 5461-10922
//	i9j0k1l2 10.0.3.1:6380 online 10923-16383
//
// ============================================================
func (c *Cluster) clusterNodes() protocol.Value {
	c.State.mu.RLock()
	defer c.State.mu.RUnlock()

	var lines []string

	for _, node := range c.State.Nodes {
		flags := node.State.String()
		if node == c.State.Self {
			flags = "myself," + flags
		}

		slotRange := node.SlotRanges()
		if slotRange == "" {
			slotRange = "-" // нет слотов
		}

		line := fmt.Sprintf("%s %s %s %s",
			node.ID,
			node.Addr,
			flags,
			slotRange,
		)
		lines = append(lines, line)
	}

	return protocol.Value{Typ: '$', Str: strings.Join(lines, "\n")}
}

// ============================================================
// clusterSlots — CLUSTER SLOTS
//
// Возвращает таблицу слотов в виде RESP-массива.
// Клиент использует это для построения таблицы маршрутизации.
//
// Формат:
//
//	[
//	  [start_slot, end_slot, [ip, port]],
//	  [start_slot, end_slot, [ip, port]],
//	  ...
//	]
//
// Пример:
//
//	[ [0, 5460, ["10.0.1.1", 6380]],
//	  [5461, 10922, ["10.0.2.1", 6380]],
//	  [10923, 16383, ["10.0.3.1", 6380]] ]
//
// ============================================================
func (c *Cluster) clusterSlots() protocol.Value {
	c.State.mu.RLock()
	defer c.State.mu.RUnlock()

	// Собираем непрерывные диапазоны слотов для каждой ноды
	type slotRange struct {
		start int
		end   int
		node  *Node
	}

	var ranges []slotRange
	var currentNode *Node
	start := -1

	for i := 0; i < TotalSlots; i++ {
		node := c.State.SlotTable[i]

		if node != currentNode {
			// Закрываем предыдущий диапазон
			if currentNode != nil && start != -1 {
				ranges = append(ranges, slotRange{start, i - 1, currentNode})
			}
			currentNode = node
			if node != nil {
				start = i
			} else {
				start = -1
			}
		}
	}

	// Последний диапазон
	if currentNode != nil && start != -1 {
		ranges = append(ranges, slotRange{start, TotalSlots - 1, currentNode})
	}

	// Формируем RESP-массив
	result := make([]protocol.Value, len(ranges))

	for i, r := range ranges {
		// Разбираем addr: "10.0.1.1:6380" → host="10.0.1.1", port=6380
		host, port := splitAddr(r.node.Addr)

		result[i] = protocol.Value{
			Typ: '*',
			Array: []protocol.Value{
				{Typ: ':', Num: r.start},
				{Typ: ':', Num: r.end},
				{Typ: '*', Array: []protocol.Value{
					{Typ: '$', Str: host},
					{Typ: ':', Num: port},
				}},
			},
		}
	}

	return protocol.Value{Typ: '*', Array: result}
}

// ============================================================
// clusterMeet — CLUSTER MEET <host> <port>
//
// Добавляет новую ноду в кластер.
// В ЭТОЙ ВЕРСИИ — просто добавляет в known_nodes.
// В Фазе 2 (gossip) — будет ещё TCP-подключение и PING.
//
// Пример:
//
//	CLUSTER MEET 10.0.2.1 6380
//	→ Добавляем Node-B в наш список нод
//
// ============================================================
func (c *Cluster) clusterMeet(args []protocol.Value) protocol.Value {
	if len(args) < 2 {
		return protocol.Value{
			Typ: '-',
			Str: "ERR wrong number of arguments for 'cluster meet' command",
		}
	}

	host := args[0].Str
	port, err := strconv.Atoi(args[1].Str)
	if err != nil {
		return protocol.Value{Typ: '-', Str: "ERR invalid port number"}
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	// Проверяем: может, эта нода уже известна?
	c.State.mu.RLock()
	for _, node := range c.State.Nodes {
		if node.Addr == addr {
			c.State.mu.RUnlock()
			return protocol.Value{Typ: '+', Str: "OK"}
		}
	}
	c.State.mu.RUnlock()

	// Создаём новую ноду
	// ID генерируем временный — в Фазе 2 (gossip) нода сообщит свой реальный ID
	newNode := NewNode(generateNodeID(), addr, port+1)

	// Опционально: CLUSTER MEET host port slot-start slot-end
	// Без gossip ноды не могут обменяться слотами, поэтому
	// задаём вручную: CLUSTER MEET 127.0.0.1 6381 5461 10922
	if len(args) >= 4 {
		slotStart, err1 := strconv.Atoi(args[2].Str)
		slotEnd, err2 := strconv.Atoi(args[3].Str)
		if err1 == nil && err2 == nil && slotStart >= 0 && slotEnd < TotalSlots {
			newNode.AssignSlots(slotStart, slotEnd)
		}
	}

	c.State.AddNode(newNode)

	return protocol.Value{Typ: '+', Str: "OK"}
}

// splitAddr разбирает "10.0.1.1:6380" на host и port.
func splitAddr(addr string) (string, int) {
	parts := strings.Split(addr, ":")
	if len(parts) != 2 {
		return addr, 0
	}

	port := 0
	fmt.Sscanf(parts[1], "%d", &port)
	return parts[0], port
}
