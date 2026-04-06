package cluster

import (
	"bufio"
	"fmt"
	"log"
	"net"
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

	// 2. Отправляем ВСЕ ключи
	count := 0
	rm.StoreForEach(func(key string, value []byte) {
		fmt.Fprintf(conn, "SET %s %s\r\n", key, string(value))
		count++
	})

	// 3. Конец полной синхронизации
	fmt.Fprintf(conn, "+FULLSYNC_DONE\r\n")

	log.Printf("[replication] Full sync done: sent %d keys to replica %s", count, replicaID)

	// 4. Сохраняем соединение для incremental sync
	rm.mu.Lock()
	rm.replicas[replicaID] = conn
	rm.mu.Unlock()
}

// ForwardWrite пересылает запись всем репликам.
// Вызывается из main.go после каждого SET/DEL.
func (rm *ReplicationManager) ForwardWrite(command string) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	for id, conn := range rm.replicas {
		_, err := fmt.Fprintf(conn, "%s\r\n", command)
		if err != nil {
			log.Printf("[replication] Failed to forward to replica %s: %v", id, err)
			// Реплика отвалилась — удалим позже
		}
	}
}

// ConnectToMaster — реплика подключается к мастеру и начинает синхронизацию.
// Запускается в горутине после CLUSTER REPLICATE.
func (rm *ReplicationManager) ConnectToMaster(masterAddr string) {
	// 1. TCP-подключение к мастеру (клиентский порт)
	conn, err := net.DialTimeout("tcp", masterAddr, 5*time.Second)
	if err != nil {
		log.Printf("[replication] Failed to connect to master %s: %v", masterAddr, err)
		return
	}
	rm.masterConn = conn
	log.Printf("[replication] Connected to master %s", masterAddr)

	// 2. Шлём PSYNC с нашим ID
	selfID := rm.cluster.State.Self.ID
	fmt.Fprintf(conn, "*2\r\n$5\r\nPSYNC\r\n$%d\r\n%s\r\n", len(selfID), selfID)

	// 3. Читаем поток данных от мастера
	scanner := bufio.NewScanner(conn)
	fullSyncDone := false

	for scanner.Scan() {
		select {
		case <-rm.stopCh:
			return
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

	if err := scanner.Err(); err != nil {
		log.Printf("[replication] Connection to master lost: %v", err)
	}
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
	}
}

// Stop останавливает репликацию.
func (rm *ReplicationManager) Stop() {
	close(rm.stopCh)
	if rm.masterConn != nil {
		rm.masterConn.Close()
	}
}
