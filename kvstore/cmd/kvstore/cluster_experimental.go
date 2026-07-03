//go:build experimental

package main

import (
	"fmt"
	"log/slog"
	"net"

	"kvstore/kvstore/internal/cluster"
	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/vector"
)

// clusterAdapter приводит *cluster.Cluster к узкому интерфейсу clusterNode.
// Компилируется только в сборке с тегом `experimental`.
type clusterAdapter struct{ c *cluster.Cluster }

func (a clusterAdapter) CheckKey(key string) *protocol.Value    { return a.c.CheckKey(key) }
func (a clusterAdapter) CheckKeyAsk(key string) *protocol.Value { return a.c.CheckKeyAsk(key) }
func (a clusterAdapter) IsReplica() bool                        { return a.c.State.Self.Role == cluster.RoleReplica }

func (a clusterAdapter) ForwardWrite(cmd string) {
	if a.c.Repl != nil {
		a.c.Repl.ForwardWrite(cmd)
	}
}

func (a clusterAdapter) HandleClusterCommand(args []protocol.Value) protocol.Value {
	return a.c.HandleClusterCommand(args)
}

func (a clusterAdapter) MigrateKey(host string, port int, key string) protocol.Value {
	return a.c.MigrateKey(host, port, key)
}

func (a clusterAdapter) HandlePsync(conn net.Conn, replicaID string) {
	a.c.Repl.HandlePsync(conn, replicaID)
}

func (a clusterAdapter) StopGossip() { a.c.StopGossip() }

// newClusterRouter поднимает кластерный узел и связывает его с локальными KV/vector/TTL.
// Вся обвязка репликации/миграции живёт здесь, под тегом `experimental`.
func newClusterRouter(addr string, gossipPort, slotStart, slotEnd int,
	s *tcmalloc.TCMallocStore, vecStore vector.VectorIndex, ttl *store.TTLManager) (clusterNode, error) {

	cl := cluster.New(addr, gossipPort)
	cl.State.Self.AssignSlots(slotStart, slotEnd)
	cl.State.RebuildSlotTable()
	slog.Info("cluster mode (experimental)", "node", cl.State.Self.ID, "slotStart", slotStart, "slotEnd", slotEnd)

	cl.GetKeysInSlotFunc = func(slot uint16, count int) []string {
		return s.GetKeysInSlot(slot, count, cluster.KeySlot)
	}
	cl.MigrateGetFunc = func(key string) ([]byte, bool) {
		return s.Get(key)
	}
	cl.MigrateDelFunc = func(key string) {
		s.Del(0, key)
		vecStore.Delete(key)
		ttl.OnDelete(key)
	}
	cl.MigrateGetVecFunc = func(key string) ([]float32, bool) {
		return vecStore.Get(key)
	}
	cl.MigrateSetRemoteVecFunc = func(addr, key string, vec []float32) error {
		return SendVectorToNode(addr, key, vec)
	}

	cl.Repl.StoreForEach = func(fn func(key string, value []byte)) { s.ForEach(fn) }
	cl.Repl.VecStoreAdd = func(key string, vec []float32) { vecStore.Add(key, vec) }
	cl.Repl.VecStoreDel = func(key string) { vecStore.Delete(key) }
	cl.Repl.VecStoreForEach = func(fn func(key string, vec []float32)) { vecStore.ForEach(fn) }
	cl.Repl.StoreSet = func(key string, value []byte) { s.Set(0, key, value) }
	cl.Repl.StoreDel = func(key string) { s.Del(0, key) }
	cl.Repl.StoreClear = func() { s.Clear() }
	cl.Repl.VecStoreClear = func() { vecStore.Clear() }

	if err := cl.StartGossip(); err != nil {
		return nil, fmt.Errorf("start gossip: %w", err)
	}
	return clusterAdapter{cl}, nil
}
