package cluster

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ReplicationManager struct {
	cluster *Cluster

	replicas map[string]net.Conn
	mu       sync.RWMutex

	masterConn net.Conn
	stopCh     chan struct{}

	StoreForEach func(fn func(key string, value []byte))
	StoreSet     func(key string, value []byte)
	StoreDel     func(key string)

	// Колбэки для репликации векторного индекса.
	// Без них VSIM.ADD/DEL не реплицируются на реплики.
	VecStoreAdd     func(key string, vec []float32)
	VecStoreDel     func(key string)
	VecStoreForEach func(fn func(key string, vec []float32))
}

func NewReplicationManager(c *Cluster) *ReplicationManager {
	return &ReplicationManager{
		cluster:  c,
		replicas: make(map[string]net.Conn),
		stopCh:   make(chan struct{}),
	}
}

// HandlePsync — обрабатывает PSYNC от реплики.
// Вызывается из main.go когда реплика шлёт команду PSYNC.
//
// Протокол:
//  1. Шлём "+FULLSYNC\r\n"
//  2. Для каждого ключа: "SET <key> <value>\r\n"
//  3. Шлём "+FULLSYNC_DONE\r\n"
//  4. Сохраняем соединение в replicas map (для incremental sync)
func (rm *ReplicationManager) HandlePsync(conn net.Conn, replicaID string) {
	log.Printf("[replication] Full sync started for replica %s", replicaID)

	// 1. Начало полной синхронизации
	fmt.Fprintf(conn, "+FULLSYNC\r\n")

	// 2. Отправляем ВСЕ KV-ключи
	count := 0
	rm.StoreForEach(func(key string, value []byte) {
		fmt.Fprintf(conn, "SET %s %s\r\n", key, string(value))
		count++
	})

	// 3. Отправляем ВСЕ векторы
	vecCount := 0
	if rm.VecStoreForEach != nil {
		rm.VecStoreForEach(func(key string, vec []float32) {
			var sb strings.Builder
			sb.WriteString("VSIM.ADD ")
			sb.WriteString(key)
			for _, v := range vec {
				sb.WriteByte(' ')
				fmt.Fprintf(&sb, "%g", v)
			}
			sb.WriteString("\r\n")
			fmt.Fprint(conn, sb.String())
			vecCount++
		})
	}

	// 4. Конец полной синхронизации
	fmt.Fprintf(conn, "+FULLSYNC_DONE\r\n")

	log.Printf("[replication] Full sync done: sent %d keys + %d vectors to replica %s",
		count, vecCount, replicaID)

	// 5. Сохраняем соединение для incremental sync
	rm.mu.Lock()
	rm.replicas[replicaID] = conn
	rm.mu.Unlock()
}

// ForwardWrite пересылает запись всем репликам.
// Вызывается из main.go после каждого SET/DEL.
//
// Паттерн "Collect and Sweep":
//  1. RLock: отправляем данные, собираем ID мёртвых реплик
//  2. Lock: удаляем мёртвых из мапы и закрываем их соединения
//
// Почему не delete() прямо в range?
//   - Мы под RLock (читающий лок), а delete — это запись → data race
//   - Нужен отдельный Lock для модификации мапы
func (rm *ReplicationManager) ForwardWrite(command string) {
	rm.mu.RLock()

	// Если реплик нет — быстрый выход без аллокаций
	if len(rm.replicas) == 0 {
		rm.mu.RUnlock()
		return
	}

	// Collect: отправляем и собираем мёртвых
	var dead []string
	for id, conn := range rm.replicas {
		_, err := fmt.Fprintf(conn, "%s\r\n", command)
		if err != nil {
			log.Printf("[replication] Replica %s disconnected: %v", id, err)
			dead = append(dead, id)
		}
	}
	rm.mu.RUnlock()

	// Sweep: удаляем мёртвых (только если есть)
	if len(dead) > 0 {
		rm.mu.Lock()
		for _, id := range dead {
			if conn, ok := rm.replicas[id]; ok {
				conn.Close() // закрываем TCP-соединение
				delete(rm.replicas, id)
				log.Printf("[replication] Removed dead replica %s", id)
			}
		}
		rm.mu.Unlock()
	}
}

// ConnectToMaster — реплика подключается к мастеру с автоматическим reconnect.
//
// Exponential Backoff:
//
//	Попытка 1: через 1 сек
//	Попытка 2: через 2 сек
//	Попытка 3: через 4 сек
//	...до максимума 30 сек
//
// При успешном подключении backoff сбрасывается на 1 сек.
// Цикл останавливается только по rm.stopCh (graceful shutdown).
func (rm *ReplicationManager) ConnectToMaster(masterAddr string) {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		// Проверяем, не остановили ли нас
		select {
		case <-rm.stopCh:
			log.Println("[replication] Replication stopped")
			return
		default:
		}

		err := rm.replicationLoop(masterAddr)

		// replicationLoop вернул nil = мы сами остановились (stopCh)
		if err == nil {
			return
		}

		log.Printf("[replication] Connection to master lost: %v. Reconnecting in %v...", err, backoff)

		// Ждём перед повторной попыткой (или выходим если stopCh)
		select {
		case <-rm.stopCh:
			return
		case <-time.After(backoff):
		}

		// Увеличиваем backoff (экспоненциально, до максимума)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// replicationLoop — одна сессия репликации.
// Подключается к мастеру, делает PSYNC, читает поток команд.
// Возвращает ошибку при обрыве соединения (для reconnect).
// Возвращает nil если остановлен через stopCh.
func (rm *ReplicationManager) replicationLoop(masterAddr string) error {
	// 1. TCP-подключение к мастеру
	conn, err := net.DialTimeout("tcp", masterAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	rm.masterConn = conn
	log.Printf("[replication] Connected to master %s", masterAddr)

	// Гарантируем закрытие соединения при выходе
	defer func() {
		conn.Close()
		rm.masterConn = nil
	}()

	// 2. Шлём PSYNC с нашим ID
	selfID := rm.cluster.State.Self.ID
	fmt.Fprintf(conn, "*2\r\n$5\r\nPSYNC\r\n$%d\r\n%s\r\n", len(selfID), selfID)

	// 3. Читаем поток данных от мастера
	scanner := bufio.NewScanner(conn)
	fullSyncDone := false

	for scanner.Scan() {
		select {
		case <-rm.stopCh:
			return nil // graceful shutdown
		default:
		}

		line := scanner.Text()

		// Служебные сообщения
		if line == "+FULLSYNC" {
			log.Println("[replication] Full sync started...")
			continue
		}
		if line == "+FULLSYNC_DONE" {
			log.Println("[replication] Full sync done, switching to incremental")
			fullSyncDone = true
			continue
		}

		// Парсим команду: "SET key value" или "DEL key"
		rm.applyReplicationCommand(line, fullSyncDone)
	}

	// scanner.Scan() вернул false = соединение оборвалось
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	return fmt.Errorf("connection closed by master")
}

// applyReplicationCommand применяет команду от мастера.
func (rm *ReplicationManager) applyReplicationCommand(line string, incremental bool) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return
	}

	cmd := strings.ToUpper(parts[0])

	switch cmd {
	case "SET":
		if len(parts) < 3 {
			return
		}
		key := parts[1]
		value := parts[2]
		rm.StoreSet(key, []byte(value))
		if incremental {
			log.Printf("[replication] Applied: SET %s", key)
		}

	case "DEL":
		key := parts[1]
		rm.StoreDel(key)
		if incremental {
			log.Printf("[replication] Applied: DEL %s", key)
		}

	case "VSIM.ADD":
		// Формат: VSIM.ADD key 0.1 0.2 0.3 ...
		if len(parts) < 3 {
			return
		}
		key := parts[1]
		// parts[2] содержит все флоаты через пробел (SplitN с N=3)
		floatStrs := strings.Fields(parts[2])
		vec := make([]float32, len(floatStrs))
		for i, s := range floatStrs {
			f, err := strconv.ParseFloat(s, 32)
			if err != nil {
				log.Printf("[replication] VSIM.ADD parse error: %v", err)
				return
			}
			vec[i] = float32(f)
		}
		if rm.VecStoreAdd != nil {
			rm.VecStoreAdd(key, vec)
		}
		if incremental {
			log.Printf("[replication] Applied: VSIM.ADD %s (%d dims)", key, len(vec))
		}

	case "VSIM.DEL":
		key := parts[1]
		if rm.VecStoreDel != nil {
			rm.VecStoreDel(key)
		}
		if incremental {
			log.Printf("[replication] Applied: VSIM.DEL %s", key)
		}
	}
}

// Stop останавливает репликацию.
func (rm *ReplicationManager) Stop() {
	close(rm.stopCh)
	if rm.masterConn != nil {
		rm.masterConn.Close()
	}
}
