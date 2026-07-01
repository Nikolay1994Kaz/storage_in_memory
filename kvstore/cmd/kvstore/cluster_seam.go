package main

import (
	"net"

	"kvstore/kvstore/internal/protocol"
)

// clusterNode — узкий интерфейс кластера, которым пользуется hot-path команд.
// Он же — шов сборки: реальная реализация (адаптер над *cluster.Cluster) живёт в
// cluster_experimental.go под тегом `experimental`, а в обычной прод-сборке
// newClusterRouter (cluster_stub.go) возвращает nil и весь ~2000-строчный
// distributed-код вообще НЕ линкуется в бинарь. Так полу-готовый кластер не
// попадает в прод и не включается случайно.
//
// nil clusterNode в hot-path = single-node: все `if cl != nil` ложны, ветки
// не берутся (предсказуемый бранч, ~0 стоимости).
type clusterNode interface {
	// CheckKey возвращает MOVED-редирект, если ключ обслуживает другой узел, иначе nil.
	CheckKey(key string) *protocol.Value
	// CheckKeyAsk возвращает ASK-редирект во время миграции слота, иначе nil.
	CheckKeyAsk(key string) *protocol.Value
	// IsReplica сообщает, что узел в роли реплики (read-only для клиентских записей).
	IsReplica() bool
	// ForwardWrite реплицирует команду на реплики. No-op, если репликация не настроена.
	ForwardWrite(cmd string)
	// HandleClusterCommand обрабатывает RESP-команду CLUSTER.
	HandleClusterCommand(args []protocol.Value) protocol.Value
	// MigrateKey переносит ключ на удалённый узел (команда MIGRATE).
	MigrateKey(host string, port int, key string) protocol.Value
	// HandlePsync обслуживает поток репликации к реплике (команда PSYNC).
	HandlePsync(conn net.Conn, replicaID string)
	// StopGossip останавливает gossip/failure-detector при завершении.
	StopGossip()
}
