package main

// Диспетчер команд: вход сервера (AUTH, подписки, транзакции), общие гейты и
// разводка команд по обработчикам семейств.

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"kvstore/kvstore/internal/monitoring"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/kv"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// commands — всё, к чему обращаются обработчики команд: хранилища, журнал,
// TTL, pub/sub, кластер и функции снапшота для COMPACT. Один экземпляр на
// процесс. Обработчики — его методы, по файлу на семейство команд:
// cmd_kv.go, cmd_zset.go, cmd_vector.go, cmd_pubsub.go, cmd_cluster.go.
type commands struct {
	kv          *kv.Store
	wal         *wal.BatchWAL
	ttl         *store.TTLManager
	hub         *pubsub.Hub
	cluster     clusterNode // nil вне кластерного режима
	vec         *vector.LeveledVectorStore
	zsets       *zset.ZSetRegistry
	iterateAll  func(fn func(op byte, key string, value []byte))
	saveVectors func() error

	// AUTH включён, если задан пароль; сравниваются SHA-256-дайджесты
	// constant-time.
	authEnabled bool
	authHash    [32]byte
}

// handle — точка входа сервера для одной команды клиента. До диспетчера
// проходят AUTH, режим подписки и транзакции (MULTI/EXEC/DISCARD).
func (c *commands) handle(cs *server.ConnState, args [][]byte) {
	if len(args) == 0 {
		cs.Buf.WriteError("ERR empty command")
		return
	}

	cmd := strings.ToUpper(string(args[0]))
	cmdArgs := args[1:]

	// ─── AUTH ───────────────────────────────────────────
	// Если --requirepass задан, клиент должен пройти AUTH
	// перед выполнением любых команд (кроме AUTH и PING).
	if c.authEnabled {
		if cmd == "AUTH" {
			if len(cmdArgs) != 1 {
				cs.Buf.WriteError("ERR wrong number of arguments for 'AUTH' command")
				return
			}
			// Сравниваем SHA-256-дайджесты constant-time: одинаковая длина
			// входа (32 байта) + subtle.ConstantTimeCompare убирают тайминг-
			// сайд-канал (утечку длины/префикса пароля при переборе).
			inHash := sha256.Sum256(cmdArgs[0])
			if subtle.ConstantTimeCompare(inHash[:], c.authHash[:]) == 1 {
				cs.Authenticated = true
				cs.Buf.WriteSimpleString("OK")
			} else {
				cs.Buf.WriteError("ERR invalid password")
			}
			return
		}
		if !cs.Authenticated && cmd != "PING" {
			cs.Buf.WriteError("NOAUTH Authentication required")
			return
		}
	}

	// Subscriber-mode (как в Redis): пока соединение имеет подписки, обслуживаем
	// его ТОЛЬКО через pub/sub-шов, где все ответы идут через единственного
	// писателя conn — writePump. Иначе основной обработчик пишет в cs.Buf
	// параллельно с writePump → два писателя в один сокет (перемешанные кадры),
	// а клиент-подписчик не ждёт обычных ответов. Первый SUBSCRIBE идёт обычным
	// путём (ещё не подписчик) и переводит соединение в этот режим.
	if c.hub.IsSubscriber(cs.Conn) {
		c.hub.HandleSubscriberCommand(cs.Conn, cmd, cmdArgs)
		return
	}

	// Транзакции
	switch cmd {
	case "MULTI":
		start := time.Now()
		cs.InTx = true
		cs.TxQueue = nil
		cs.TxAborted = false
		cs.Buf.WriteSimpleString("OK")
		monitoring.RecordCommand(cmd, time.Since(start))
		return
	case "DISCARD":
		start := time.Now()
		if !cs.InTx {
			cs.Buf.WriteError("ERR DISCARD without MULTI")
			monitoring.RecordCommand(cmd, time.Since(start))
			return
		}
		cs.InTx = false
		cs.TxQueue = nil
		cs.TxAborted = false
		cs.Buf.WriteSimpleString("OK")
		monitoring.RecordCommand(cmd, time.Since(start))
		return
	case "EXEC":
		startEXEC := time.Now()
		if !cs.InTx {
			cs.Buf.WriteError("ERR EXEC without MULTI")
			monitoring.RecordCommand(cmd, time.Since(startEXEC))
			return
		}
		// H2: если в очередь попала запрещённая команда — вся транзакция
		// отменяется (как EXECABORT в Redis). Не выполняем ничего.
		if cs.TxAborted {
			cs.Buf.WriteError("EXECABORT Transaction discarded because of previous errors.")
			cs.InTx = false
			cs.TxQueue = nil
			cs.TxAborted = false
			monitoring.RecordCommand(cmd, time.Since(startEXEC))
			return
		}
		// M2 (осознанно НЕ фиксим — задокументировано в README «Isolation
		// contract»): execQueuedTx берёт globalTxMu, но тот сериализует лишь
		// EXEC-vs-EXEC. Обычная команда с другого соединения может вклиниться
		// МЕЖДУ командами очереди → EXEC даёт группировку и durability, но НЕ
		// isolation. Настоящая изоляция потребовала бы глобальной сериализации
		// всех записей на время EXEC. Контракт сознательно сужен, а не расширен.
		execQueuedTx(cs.TxQueue, cs.Buf.WriteArrayHeader, func(qCmd string, qCmdArgs [][]byte) {
			startCmd := time.Now()
			c.execute(cs, qCmd, qCmdArgs)
			monitoring.RecordCommand(qCmd, time.Since(startCmd))
		})
		cs.InTx = false
		cs.TxQueue = nil
		monitoring.RecordCommand(cmd, time.Since(startEXEC))
		return
	}

	if cs.InTx {
		queueTxCommand(cs, args, cmd)
		return
	}

	start := time.Now()
	c.execute(cs, cmd, cmdArgs)
	monitoring.RecordCommand(cmd, time.Since(start))
}

// execute выполняет одну команду: сначала общие гейты (память, режим
// восстановления, исправность журнала), затем обработчик семейства.
func (c *commands) execute(cs *server.ConnState, cmd string, args [][]byte) {
	buf := cs.Buf

	// OOM-гейт (единая точка для всех растущих в памяти команд). Раньше проверка
	// висела только на SET — LPUSH/HSET-класс и, главное, ZADD/VSIM.ADD/VSIM.ADDBIN
	// шли мимо неё и уводили процесс в OOM. Удаляющие/читающие команды (DEL,
	// VSIM.DEL, ZREM, GET…) НЕ блокируются — иначе из OOM не выйти освобождением.
	// Покрывает и прямой путь, и EXEC (обе ветки зовут execute).
	if isMemoryGrowingCmd(cmd) && c.kv.IsOOM() {
		monitoring.OomEvents.Inc()
		buf.WriteError("OOM command not allowed when used memory > 'maxmemory'")
		return
	}

	// Восстановленный на момент узел отвечает только на чтение. Любая запись
	// породила бы вторую историю, и дальше уже не отличить её от настоящей;
	// COMPACT отдельно, потому что он не «мутация состояния», а перезапись
	// снапшотов в КАТАЛОГЕ ДАННЫХ — то есть порча оригинала.
	if restoreLSN > 0 && (isWriteCmd(cmd) || cmd == "COMPACT") {
		buf.WriteError(fmt.Sprintf("READONLY point-in-time restore to LSN %d serves reads only", restoreLSN))
		return
	}
	// Durability fail-stop: если WAL перестал durable-писать на диск (ENOSPC,
	// I/O error), мы больше не можем честно подтверждать мутации. Отклоняем ВСЕ
	// пишущие команды — включая удаляющие (в отличие от OOM-гейта выше), т.к. на
	// полном диске нельзя записать даже удаление. Чтение остаётся доступным,
	// чтобы клиенты могли снять данные. Аналог Redis stop-writes-on-bgsave-error:
	// лучше явная ошибка, чем тихая потеря уже подтверждённой записи.
	if isWriteCmd(cmd) {
		if err := c.wal.Failed(); err != nil {
			monitoring.WalFailStop.Inc()
			buf.WriteError("WAL persistence failed, writes are blocked (durability fail-stop): " + err.Error())
			return
		}
	}

	switch cmd {
	case "PING":
		c.cmdPing(cs, args)
	case "CLUSTER":
		c.cmdCluster(cs, args)
	case "MIGRATE":
		c.cmdMigrate(cs, args)
	case "PSYNC":
		c.cmdPsync(cs, args)
	case "SET":
		c.cmdSet(cs, args)
	case "GET":
		c.cmdGet(cs, args)
	case "DEL":
		c.cmdDel(cs, args)
	case "EXPIRE":
		c.cmdExpire(cs, args)
	case "TTL":
		c.cmdTTL(cs, args)
	case "PERSIST":
		c.cmdPersist(cs, args)
	case "SUBSCRIBE":
		c.cmdSubscribe(cs, args)
	case "UNSUBSCRIBE":
		c.cmdUnsubscribe(cs, args)
	case "PUBLISH":
		c.cmdPublish(cs, args)
	case "DBSIZE":
		c.cmdDBSize(cs, args)
	case "COMPACT":
		c.cmdCompact(cs, args)
	case "VSIM.ADDBIN":
		c.cmdVSimAddBin(cs, args)
	case "VSIM.ADD":
		c.cmdVSimAdd(cs, args)
	case "VSIM.ADDATTR":
		c.cmdVSimAddAttr(cs, args)
	case "VSIM.ADDDOC":
		c.cmdVSimAddDoc(cs, args)
	case "VSIM.DEL":
		c.cmdVSimDel(cs, args)
	case "VSIM.EXISTS":
		c.cmdVSimExists(cs, args)
	case "VSIM.SEARCH":
		c.cmdVSimSearch(cs, args)
	case "VSIM.SEARCHTEXT":
		c.cmdVSimSearchText(cs, args)
	case "VSIM.HYBRID":
		c.cmdVSimHybrid(cs, args)
	case "VSIM.SEARCHBIN":
		c.cmdVSimSearchBin(cs, args)
	case "VSIM.FILTER":
		c.cmdVSimFilter(cs, args)
	case "VSIM.INFO":
		c.cmdVSimInfo(cs, args)
	case "VSIM.SEARCHFILTER":
		c.cmdVSimSearchFilter(cs, args)
	case "ZADD":
		c.cmdZAdd(cs, args)
	case "ZSCORE":
		c.cmdZScore(cs, args)
	case "ZREM":
		c.cmdZRem(cs, args)
	case "ZRANGEBYSCORE":
		c.cmdZRangeByScore(cs, args)
	case "ZCARD":
		c.cmdZCard(cs, args)
	case "VSIM.SEARCHRANGE":
		c.cmdVSimSearchRange(cs, args)
	default:
		buf.WriteError(fmt.Sprintf("ERR unknown command '%s'", cmd))
	}
}

// isMemoryGrowingCmd сообщает, увеличивает ли команда использование памяти.
// Под OOM-гейт попадают только такие команды; удаляющие/читающие (DEL, VSIM.DEL,
// ZREM, EXPIRE, GET…) всегда разрешены — чтобы из состояния OOM можно было выйти
// освобождением памяти, а не заблокировать себе единственный путь наружу.
func isMemoryGrowingCmd(cmd string) bool {
	switch cmd {
	case "SET", "ZADD", "VSIM.ADD", "VSIM.ADDBIN", "VSIM.ADDATTR", "VSIM.ADDDOC":
		return true
	}
	return false
}

// isWriteCmd сообщает, мутирует ли команда состояние и потому требует durable
// записи в WAL. Используется durability fail-stop гейтом: при сломанном WAL
// (ENOSPC/I/O error) ВСЕ такие команды отклоняются — включая удаляющие
// (DEL/VSIM.DEL/ZREM/EXPIRE/PERSIST), потому что удаление тоже надо записать в
// лог, иначе оно «воскреснет» после рестарта. Это отличается от OOM-гейта
// (isMemoryGrowingCmd), где удаления РАЗРЕШЕНЫ — там цель освободить память,
// а здесь диск не может принять вообще ничего. Чтение (GET/…) не затронуто.
func isWriteCmd(cmd string) bool {
	switch cmd {
	case "SET", "DEL", "EXPIRE", "PERSIST",
		"VSIM.ADD", "VSIM.ADDBIN", "VSIM.ADDATTR", "VSIM.ADDDOC", "VSIM.DEL",
		"ZADD", "ZREM":
		return true
	}
	return false
}

var globalTxMu sync.Mutex

// execQueuedTx выполняет очередь команд MULTI/EXEC под globalTxMu.
//
// H1: defer Unlock ОБЯЗАТЕЛЕН. Если run (execute) паникует на битой команде в
// очереди, её ловит recover в горутине соединения (server.serve) — но Unlock
// без defer был бы пропущен → globalTxMu залочен НАВСЕГДА → EXEC всех
// соединений виснут. defer освобождает мьютекс на разворачивании паники,
// после чего она уходит в recover (соединение закрывается).
func execQueuedTx(queue [][][]byte, writeHeader func(int), run func(qCmd string, qCmdArgs [][]byte)) {
	globalTxMu.Lock()
	defer globalTxMu.Unlock()

	writeHeader(len(queue))
	for _, queuedArgs := range queue {
		qCmd := strings.ToUpper(string(queuedArgs[0]))
		run(qCmd, queuedArgs[1:])
	}
}

// queueTxCommand ставит команду в очередь транзакции ИЛИ отклоняет её (H2).
//
// Pub/sub subscribe-команды (forbiddenInTx) переводят соединение в subscriber-
// mode и пишут ответы мимо cs.Buf → внутри EXEC это укоротило бы обещанный
// ArrayHeader(N) → RESP-десинк соединения навсегда. Такую команду НЕ ставим в
// очередь, а помечаем транзакцию на отмену (cs.TxAborted) — как EXECABORT в
// Redis: последующий EXEC ничего не выполнит и вернёт ошибку.
func queueTxCommand(cs *server.ConnState, args [][]byte, cmd string) {
	if forbiddenInTx(cmd) {
		cs.TxAborted = true
		cs.Buf.WriteError("ERR " + cmd + " is not allowed in transactions")
		return
	}
	// Копируем args — ring buffer будет перезаписан!
	argsCopy := make([][]byte, len(args))
	for i, a := range args {
		argsCopy[i] = append([]byte(nil), a...)
	}
	cs.TxQueue = append(cs.TxQueue, argsCopy)
	cs.Buf.WriteSimpleString("QUEUED")
}

// forbiddenInTx — команды, недопустимые внутри MULTI/EXEC (H2).
//
// Pub/sub subscribe-команды переводят соединение в subscriber-mode: запускают
// writePump (второй писатель в сокет) и шлют ответы через pub/sub-канал, а не в
// cs.Buf. Внутри EXEC это ломает RESP-кадр (обещанный ArrayHeader(N) получает
// меньше элементов) и рвёт соединение. Redis тоже запрещает их в транзакции.
func forbiddenInTx(cmd string) bool {
	switch cmd {
	case "SUBSCRIBE", "UNSUBSCRIBE":
		return true
	}
	return false
}

func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}
