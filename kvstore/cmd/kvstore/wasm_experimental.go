//go:build experimental

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"kvstore/kvstore/internal/compute"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/wal"
)

// realCompute — адаптер над *compute.Engine + TriggerManager, реализующий
// computeRuntime. Линкуется только с тегом experimental: именно здесь (и только
// здесь) в бинарь попадают пакет compute и wazero-рантайм.
type realCompute struct {
	engine   *compute.Engine
	triggers *compute.TriggerManager
}

// newComputeEngine создаёт WASM-движок и подключает все мостики к основному
// движку (KV/Vector/PubSub/WAL). Перенесено из main() дословно — семантика та же.
func newComputeEngine(deps computeDeps) computeRuntime {
	engine := compute.NewEngine()

	engine.GlobalLock = deps.globalMu.Lock
	engine.GlobalUnlock = deps.globalMu.Unlock

	engine.StoreGet = func(key string) ([]byte, bool) {
		return deps.store.Get(key)
	}
	engine.StoreSet = func(workerID int, key string, value []byte) {
		deps.store.Set(workerID, key, value)
	}
	engine.StoreDel = func(workerID int, key string) {
		deps.store.Del(workerID, key)
	}
	engine.Publish = func(channel, message string) {
		deps.hub.Publish(channel, message)
	}
	engine.VSimSearch = func(workerID int, query []float32, K int) []struct {
		Key      string
		Distance float32
	} {
		results, err := deps.vecStore.Search(query, K, nil)
		if err != nil {
			return nil
		}
		out := make([]struct {
			Key      string
			Distance float32
		}, len(results))
		for i, r := range results {
			out[i].Key = r.Key
			out[i].Distance = r.Distance
		}
		return out
	}

	engine.StoreSetWithWAL = func(workerID int, key string, value []byte) error {
		deps.bw.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
		deps.store.Set(workerID, key, value)
		return nil
	}
	engine.StoreDelWithWAL = func(workerID int, key string) error {
		deps.bw.Write(wal.Entry{Op: wal.OpDel, Key: key})
		deps.store.Del(workerID, key)
		deps.ttl.OnDelete(key)
		return nil
	}

	// Загрузка WASM-модулей и триггеров с диска.
	triggers := compute.NewTriggerManager(engine)
	compute.LoadAll(dataDir, engine, triggers)

	return &realCompute{engine: engine, triggers: triggers}
}

func (c *realCompute) FireSet(key string, workerID int) {
	c.triggers.Fire(compute.OnSet, key, workerID)
}

func (c *realCompute) FireDel(key string, workerID int) {
	c.triggers.Fire(compute.OnDel, key, workerID)
}

func (c *realCompute) SetAI(embed func(context.Context, string) ([]float32, error), chat func(context.Context, string) (string, error)) {
	c.engine.AIEmbed = embed
	c.engine.AIChat = chat
}

func (c *realCompute) Close() {
	c.engine.Close()
}

// HandleCommand обслуживает WASM.* команды. Тела перенесены из executeCommand
// без изменений семантики (wasm→c.engine, triggers→c.triggers).
func (c *realCompute) HandleCommand(cmd string, args [][]byte, buf *server.ConnBuf) {
	switch cmd {
	case "WASM.LOAD":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.LOAD'")
			return
		}
		name := string(args[0])
		wasmBytes := make([]byte, len(args[1]))
		copy(wasmBytes, args[1])
		if err := c.engine.LoadModule(name, wasmBytes); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		compute.SaveModule(dataDir, name, wasmBytes)
		buf.WriteSimpleString("OK")

	case "WASM.LOADFILE":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.LOADFILE'")
			return
		}
		name := string(args[0])
		filePath := string(args[1])
		wasmBytes, err := os.ReadFile(filePath)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR cannot read file: %v", err))
			return
		}
		if err := c.engine.LoadModule(name, wasmBytes); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		compute.SaveModule(dataDir, name, wasmBytes)
		buf.WriteSimpleString(fmt.Sprintf("OK loaded %d bytes", len(wasmBytes)))

	case "WASM.DROP":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.DROP'")
			return
		}
		name := string(args[0])
		if err := c.engine.DropModule(name); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		compute.DeleteModule(dataDir, name)
		buf.WriteSimpleString("OK")

	case "WASM.LIST":
		names := c.engine.ListModules()
		buf.WriteArrayHeader(len(names))
		for _, n := range names {
			buf.WriteBulkString(n)
		}

	case "WASM.EXEC":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.EXEC'")
			return
		}
		results, err := c.engine.ExecFunction(string(args[0]), string(args[1]))
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		if len(results) > 0 {
			buf.WriteInt(int(results[0]))
		} else {
			buf.WriteSimpleString("OK")
		}

	case "WASM.INFO":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.INFO'")
			return
		}
		name := string(args[0])
		loadedAt, execCount, found := c.engine.ModuleInfo(name)
		if !found {
			buf.WriteError(fmt.Sprintf("ERR module '%s' not found", name))
			return
		}
		info := fmt.Sprintf("module:%s loaded_at:%s exec_count:%d",
			name, loadedAt.Format(time.RFC3339), execCount)
		buf.WriteBulkString(info)

	case "WASM.TRIGGER":
		if len(args) < 4 {
			buf.WriteError("ERR usage: WASM.TRIGGER <SET|DEL> <pattern> <module> <func>")
			return
		}
		event := compute.TriggerEvent(strings.ToUpper(string(args[0])))
		pattern := string(args[1])
		moduleName := string(args[2])
		funcName := string(args[3])
		id := c.triggers.AddTrigger(event, pattern, moduleName, funcName)
		compute.SaveTriggers(dataDir, c.triggers)
		buf.WriteSimpleString(id)

	case "WASM.UNTRIGGER":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.UNTRIGGER'")
			return
		}
		if c.triggers.RemoveTrigger(string(args[0])) {
			compute.SaveTriggers(dataDir, c.triggers)
			buf.WriteSimpleString("OK")
		} else {
			buf.WriteError("ERR trigger not found")
		}

	case "WASM.TRIGGERS":
		all := c.triggers.ListTriggers()
		buf.WriteArrayHeader(len(all))
		for _, t := range all {
			buf.WriteBulkString(fmt.Sprintf("%s %s %s %s.%s", t.ID, t.Event, t.Pattern, t.ModuleName, t.FuncName))
		}

	default:
		buf.WriteError(fmt.Sprintf("ERR unknown command '%s'", cmd))
	}
}
