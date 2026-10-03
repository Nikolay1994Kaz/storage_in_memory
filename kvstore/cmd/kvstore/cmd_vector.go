package main

// Команды векторного и полнотекстового поиска (VSIM.*): вставка, удаление,
// поиск, фильтры и соединения с KV и sorted set.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"kvstore/kvstore/internal/monitoring"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// cmdVSimAddBin — VSIM.ADDBIN key <float32 LE>: вставка вектора в бинарном виде.
func (c *commands) cmdVSimAddBin(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) != 2 {
		buf.WriteError("ERR usage: VSIM.ADDBIN <key> <binary_vec_bytes>")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	if len(args[1])%4 != 0 {
		buf.WriteError("ERR invalid binary vector: byte length must be a multiple of 4")
		return
	}
	// Copy bytes from ring buffer for asynchronous WAL logging
	walValue := make([]byte, len(args[1]))
	copy(walValue, args[1])

	// Zero-copy cast to []float32
	vec := vector.DeserializeVectorZeroCopy(walValue)

	// Санитизация ДО записи в WAL: пустой вектор (div-by-zero → crash-loop),
	// NaN/Inf (отрава SQ8), пустой/слишком длинный ключ. Иначе отрава
	// переживёт рестарт через реплей.
	if err := vector.ValidateKey(key); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	if err := vector.ValidateVector(vec); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}

	monitoring.VectorAddTotal.Inc()
	addStart := time.Now()
	// Add ДО bw.Write (watermark-safety, как у VSIM.DEL). Если между Write и Add
	// сработает saveVectors (watermark=LastLSN + FlushDeltaSync), вектор с
	// LSN ≤ watermark ещё НЕ в дельте → снапшот без него, а реплей пропустит его
	// (LSN ≤ watermark) → потеря. Add-then-Write: незалогированный Add теряется
	// лишь вместе с крахом (консистентно), залогированный всегда переигрывается.
	// Заодно ошибка Add больше не отравляет WAL.
	if err := c.vec.Add(key, vec); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	monitoring.VectorAddDuration.Update(time.Since(addStart).Seconds())
	c.wal.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
	if c.cluster != nil {
		// Forward replication command in text format to replica nodes
		// to avoid breaking existing replica replication protocols.
		var sb strings.Builder
		sb.WriteString("VSIM.ADD ")
		sb.WriteString(key)
		for _, v := range vec {
			sb.WriteByte(' ')
			sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
		}
		c.cluster.ForwardWrite(sb.String())
	}
	buf.WriteSimpleString("OK")
}

// cmdVSimAdd — VSIM.ADD key v1 … vN.
func (c *commands) cmdVSimAdd(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 2 {
		buf.WriteError("ERR usage: VSIM.ADD <key> <v1> <v2> ... <vN>")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	vec := make([]float32, len(args)-1)
	for i := 1; i < len(args); i++ {
		f, err := strconv.ParseFloat(unsafeString(args[i]), 32)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
			return
		}
		vec[i-1] = float32(f)
	}
	// Санитизация ДО записи в WAL (см. VSIM.ADDBIN): недоверенный вход не
	// должен отравлять WAL/сегмент. ParseFloat пропускает NaN/Inf — ловим тут.
	if err := vector.ValidateKey(key); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	if err := vector.ValidateVector(vec); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	walValue := vector.SerializeVector(vec)
	monitoring.VectorAddTotal.Inc()
	addStart := time.Now()
	// Add ДО bw.Write (watermark-safety, см. VSIM.ADDBIN).
	if err := c.vec.Add(key, vec); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	monitoring.VectorAddDuration.Update(time.Since(addStart).Seconds())
	c.wal.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
	if c.cluster != nil {
		// Формат: VSIM.ADD key 0.1 0.2 0.3 ...
		var sb strings.Builder
		sb.WriteString("VSIM.ADD ")
		sb.WriteString(key)
		for _, v := range vec {
			sb.WriteByte(' ')
			sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
		}
		c.cluster.ForwardWrite(sb.String())
	}
	buf.WriteSimpleString("OK")
}

// cmdVSimAddAttr — VSIM.ADDATTR key [CAT k v]… [NUM k v]… VEC v1 … vN: вектор с атрибутами.
func (c *commands) cmdVSimAddAttr(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// VSIM.ADDATTR <key> [CAT <k> <v>]... [NUM <k> <v>]... VEC <f1> ... <fN>
	// Ингест вектора с колоночными атрибутами (tenant/категории/числа). Пишет
	// OpVSimAddAttrs → атрибуты durable через WAL (как через снапшот). Это
	// сетевой вход в colоночный tenant/attr-слой (SearchFilter/tenant-раскладка).
	if len(args) < 3 {
		buf.WriteError("ERR usage: VSIM.ADDATTR <key> [CAT k v]... [NUM k v]... VEC <v1> ... <vN>")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	attrs := vector.Attributes{Cat: map[string]string{}, Num: map[string]float64{}}
	var vec []float32
	parseErr := ""
addAttrParse:
	for i := 1; i < len(args); {
		switch strings.ToUpper(string(args[i])) {
		case "CAT":
			if i+2 >= len(args) {
				parseErr = "CAT requires <attr> <value>"
				break addAttrParse
			}
			attrs.Cat[string(args[i+1])] = string(args[i+2])
			i += 3
		case "NUM":
			if i+2 >= len(args) {
				parseErr = "NUM requires <attr> <value>"
				break addAttrParse
			}
			f, err := strconv.ParseFloat(unsafeString(args[i+2]), 64)
			if err != nil {
				parseErr = "NUM value not a float"
				break addAttrParse
			}
			attrs.Num[string(args[i+1])] = f
			i += 3
		case "VEC":
			vec = make([]float32, 0, len(args)-i-1)
			for j := i + 1; j < len(args); j++ {
				f, err := strconv.ParseFloat(unsafeString(args[j]), 32)
				if err != nil {
					parseErr = fmt.Sprintf("invalid float %q", unsafeString(args[j]))
					break addAttrParse
				}
				vec = append(vec, float32(f))
			}
			break addAttrParse
		default:
			parseErr = fmt.Sprintf("unexpected token %q (want CAT|NUM|VEC)", unsafeString(args[i]))
			break addAttrParse
		}
	}
	if parseErr != "" {
		buf.WriteError("ERR " + parseErr)
		return
	}
	if err := vector.ValidateKey(key); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	if err := vector.ValidateVector(vec); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	monitoring.VectorAddTotal.Inc()
	// Add ДО bw.Write (watermark-safety, как VSIM.ADD): вектор в дельте раньше,
	// чем LSN покрыт снапшот-watermark'ом → нет потери на рестарте.
	if err := c.vec.AddWithAttrs(key, vec, attrs); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	c.wal.Write(wal.Entry{Op: wal.OpVSimAddAttrs, Key: key, Value: vector.SerializeVectorWithAttrs(vec, attrs)})
	// Репликация ADDATTR в кластере пока не проброшена (cluster experimental,
	// attrs+legacy-replica-протокол не определён) — CheckKey-редирект работает.
	buf.WriteSimpleString("OK")
}

// cmdVSimAddDoc — VSIM.ADDDOC key TEXT text [TITLE title] [CAT k v]… [NUM k v]… VEC v1 … vN:
// документ — вектор, атрибуты и текст для BM25.
func (c *commands) cmdVSimAddDoc(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// VSIM.ADDDOC <key> TEXT <text> [TITLE <title>] [CAT <k> <v>]... [NUM <k> <v>]... VEC <f1> ... <fN>
	// Ингест дока: вектор + атрибуты + текст (BM25). Текст токенизируется
	// ЗДЕСЬ, ровно один раз: одни и те же термы уходят в дельту (AddDocTerms)
	// и в WAL (OpVSimAddDoc) — реплей НЕ перетокенизирует, журнал везёт
	// результат токенизации (бит-в-бит независимо от версии стеммера).
	// TITLE — опциональный буст заголовка («бедный BM25F», вес зашит в
	// vector.TokenizeDocTitled); вес вшивается в термы на ингесте.
	// Пустой TEXT "" снимает текст дока (семантика upsert, как у attrs).
	if len(args) < 3 {
		buf.WriteError("ERR usage: VSIM.ADDDOC <key> TEXT <text> [TITLE <title>] [CAT k v]... [NUM k v]... VEC <v1> ... <vN>")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	attrs := vector.Attributes{Cat: map[string]string{}, Num: map[string]float64{}}
	var vec []float32
	text, title := "", ""
	textSeen := false
	parseErr := ""
addDocParse:
	for i := 1; i < len(args); {
		switch strings.ToUpper(string(args[i])) {
		case "TEXT":
			if i+1 >= len(args) {
				parseErr = "TEXT requires <text>"
				break addDocParse
			}
			text = string(args[i+1])
			textSeen = true
			i += 2
		case "TITLE":
			if i+1 >= len(args) {
				parseErr = "TITLE requires <title>"
				break addDocParse
			}
			title = string(args[i+1])
			i += 2
		case "CAT":
			if i+2 >= len(args) {
				parseErr = "CAT requires <attr> <value>"
				break addDocParse
			}
			attrs.Cat[string(args[i+1])] = string(args[i+2])
			i += 3
		case "NUM":
			if i+2 >= len(args) {
				parseErr = "NUM requires <attr> <value>"
				break addDocParse
			}
			f, err := strconv.ParseFloat(unsafeString(args[i+2]), 64)
			if err != nil {
				parseErr = "NUM value not a float"
				break addDocParse
			}
			attrs.Num[string(args[i+1])] = f
			i += 3
		case "VEC":
			vec = make([]float32, 0, len(args)-i-1)
			for j := i + 1; j < len(args); j++ {
				f, err := strconv.ParseFloat(unsafeString(args[j]), 32)
				if err != nil {
					parseErr = fmt.Sprintf("invalid float %q", unsafeString(args[j]))
					break addDocParse
				}
				vec = append(vec, float32(f))
			}
			break addDocParse
		default:
			parseErr = fmt.Sprintf("unexpected token %q (want TEXT|TITLE|CAT|NUM|VEC)", unsafeString(args[i]))
			break addDocParse
		}
	}
	if parseErr == "" && !textSeen {
		parseErr = "TEXT is required (use VSIM.ADDATTR for text-less vectors)"
	}
	if parseErr != "" {
		buf.WriteError("ERR " + parseErr)
		return
	}
	if err := vector.ValidateKey(key); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	if err := vector.ValidateVector(vec); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	monitoring.VectorAddTotal.Inc()
	terms := vector.TokenizeDocTitled(title, text)
	// Add ДО bw.Write (watermark-safety, как VSIM.ADD/ADDATTR): док в дельте
	// раньше, чем LSN покрыт снапшот-watermark'ом → нет потери на рестарте.
	if err := c.vec.AddDocTerms(key, vec, attrs, terms); err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	c.wal.Write(wal.Entry{Op: wal.OpVSimAddDoc, Key: key, Value: vector.SerializeVectorWithDoc(vec, attrs, terms)})
	// Репликация в кластере не проброшена — как у ADDATTR (CheckKey-редирект есть).
	buf.WriteSimpleString("OK")
}

// cmdVSimDel — VSIM.DEL key.
func (c *commands) cmdVSimDel(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 1 {
		buf.WriteError("ERR usage: VSIM.DEL <key>")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	monitoring.VectorDeleteTotal.Inc()
	if c.vec.Delete(key) {
		c.wal.Write(wal.Entry{Op: wal.OpVSimDel, Key: key})
		if c.cluster != nil {
			c.cluster.ForwardWrite("VSIM.DEL " + key)
		}
		buf.WriteInt(1)
	} else {
		buf.WriteInt(0)
	}
}

// cmdVSimExists — VSIM.EXISTS key: точная проверка наличия, мимо ANN-поиска.
func (c *commands) cmdVSimExists(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// Прямой existence-чек мимо ANN-поиска: точечный Get по дельте/tombstones/
	// сегментам. Нужен оракулу durability (soak), чтобы разносить «вектор
	// реально потерян» от «recall-промах графа»: VSIM.SEARCH аппроксимативен
	// и может не найти даже присутствующий вектор.
	if len(args) < 1 {
		buf.WriteError("ERR usage: VSIM.EXISTS <key>")
		return
	}
	key := string(args[0])
	if c.cluster != nil && !c.cluster.IsReplica() {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	if _, ok := c.vec.Get(key); ok {
		buf.WriteInt(1)
	} else {
		buf.WriteInt(0)
	}
}

// cmdVSimSearch — VSIM.SEARCH K v1 … vN: K ближайших векторов.
func (c *commands) cmdVSimSearch(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 2 {
		buf.WriteError("ERR usage: VSIM.SEARCH <K> <v1> <v2> ... <vN>")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	query := make([]float32, len(args)-1)
	for i := 1; i < len(args); i++ {
		f, err := strconv.ParseFloat(unsafeString(args[i]), 32)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
			return
		}
		query[i-1] = float32(f)
	}
	monitoring.VectorSearchTotal.Inc()
	searchStart := time.Now()
	results, err := c.vec.Search(query, K, nil)
	monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
	}
}

// cmdVSimSearchText — VSIM.SEARCHTEXT K query [EQ attr val]… [RANGE attr lo hi]…: BM25.
func (c *commands) cmdVSimSearchText(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// VSIM.SEARCHTEXT <K> <query> [EQ <attr> <val>]... [RANGE <attr> <lo> <hi>]...
	// Лексический BM25 top-K, embedder-free путь («шаг 0» без эмбеддера).
	// EQ/RANGE (синтаксис VSIM.FILTER) судятся ДО
	// формирования top-K (пре-фильтр); статистика BM25 глобальная.
	// Ответ зеркалит VSIM.SEARCH: пары [key, score, ...], но score — BM25
	// (БОЛЬШЕ = лучше, не дистанция).
	if len(args) < 2 {
		buf.WriteError("ERR usage: VSIM.SEARCHTEXT <K> <query> [EQ attr val]... [RANGE attr lo hi]...")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	f, next, perr := parseAttrFilter(args, 2)
	if perr == "" && next != len(args) {
		perr = fmt.Sprintf("unexpected token %q (want EQ|RANGE)", unsafeString(args[next]))
	}
	if perr != "" {
		buf.WriteError("ERR " + perr)
		return
	}
	monitoring.VectorSearchTotal.Inc()
	searchStart := time.Now()
	results, err := c.vec.SearchTextFilter(unsafeString(args[1]), K, f)
	monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Score))
	}
}

// cmdVSimHybrid — VSIM.HYBRID K TEXT query [EQ …]… [RANGE …]… VEC v1 … vN: BM25 и вектор,
// слияние по рангу (RRF).
func (c *commands) cmdVSimHybrid(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// VSIM.HYBRID <K> TEXT <query> [EQ <attr> <val>]... [RANGE <attr> <lo> <hi>]... VEC <v1> ... <vN>
	// Гибрид: top-100 лексический + top-100 векторный → Reciprocal Rank
	// Fusion (k=60, docs/BM25_HYBRID_DESIGN.md). EQ/RANGE применяются к
	// ОБОИМ плечам ДО RRF (filter-then-fuse) — отсев после слияния морил
	// бы голодом маленький тенант. Ответ
	// [key, rrf_score, ...]; RRF-скор НЕ сравним ни с BM25, ни с
	// дистанцией — полезен только порядок.
	if len(args) < 5 || strings.ToUpper(string(args[1])) != "TEXT" {
		buf.WriteError("ERR usage: VSIM.HYBRID <K> TEXT <query> [EQ attr val]... [RANGE attr lo hi]... VEC <v1> ... <vN>")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	f, next, perr := parseAttrFilter(args, 3)
	if perr == "" && (next >= len(args) || strings.ToUpper(string(args[next])) != "VEC") {
		perr = "missing VEC section"
	}
	if perr != "" {
		buf.WriteError("ERR " + perr)
		return
	}
	query := make([]float32, 0, len(args)-next-1)
	for i := next + 1; i < len(args); i++ {
		fv, err := strconv.ParseFloat(unsafeString(args[i]), 32)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
			return
		}
		query = append(query, float32(fv))
	}
	monitoring.VectorSearchTotal.Inc()
	searchStart := time.Now()
	results, err := c.vec.SearchHybridFilter(unsafeString(args[2]), query, K, f)
	monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Score))
	}
}

// cmdVSimSearchBin — VSIM.SEARCHBIN K <float32 LE>: поиск по запросу в бинарном виде.
func (c *commands) cmdVSimSearchBin(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) != 2 {
		buf.WriteError("ERR usage: VSIM.SEARCHBIN <K> <binary_vec_bytes>")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	if len(args[1])%4 != 0 {
		buf.WriteError("ERR invalid binary vector: byte length must be a multiple of 4")
		return
	}

	// Zero-copy cast directly from the read buffer
	query := vector.DeserializeVectorZeroCopy(args[1])

	monitoring.VectorSearchTotal.Inc()
	searchStart := time.Now()
	results, err := c.vec.Search(query, K, nil)
	monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
	}
}

// cmdVSimFilter — VSIM.FILTER K [EQ attr val]… [RANGE attr lo hi]… VEC v1 … vN: поиск с
// фильтром по колонкам атрибутов.
func (c *commands) cmdVSimFilter(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// VSIM.FILTER <K> [EQ <attr> <val>]... [RANGE <attr> <lo> <hi>]... VEC <f1> ... <fN>
	// Колоночный фильтр-поиск: EQ (категориальное равенство) и RANGE (числовой
	// диапазон) по attr-колонкам + tenant-роутинг, если partitionAttr задан в EQ.
	// Это сетевой вход в LeveledVectorStore.SearchFilter (в отличие от KV-метаданного
	// VSIM.SEARCHFILTER, который дёргает GET на каждого кандидата).
	if len(args) < 3 {
		buf.WriteError("ERR usage: VSIM.FILTER <K> [EQ attr val]... [RANGE attr lo hi]... VEC <v1> ... <vN>")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	f := vector.Filter{Eq: map[string]string{}, Range: map[string][2]float64{}}
	var query []float32
	parseErr := ""
filterParse:
	for i := 1; i < len(args); {
		switch strings.ToUpper(string(args[i])) {
		case "EQ":
			if i+2 >= len(args) {
				parseErr = "EQ requires <attr> <value>"
				break filterParse
			}
			f.Eq[string(args[i+1])] = string(args[i+2])
			i += 3
		case "RANGE":
			if i+3 >= len(args) {
				parseErr = "RANGE requires <attr> <lo> <hi>"
				break filterParse
			}
			lo, e1 := strconv.ParseFloat(unsafeString(args[i+2]), 64)
			hi, e2 := strconv.ParseFloat(unsafeString(args[i+3]), 64)
			if e1 != nil || e2 != nil {
				parseErr = "RANGE lo/hi not floats"
				break filterParse
			}
			f.Range[string(args[i+1])] = [2]float64{lo, hi}
			i += 4
		case "VEC":
			query = make([]float32, 0, len(args)-i-1)
			for j := i + 1; j < len(args); j++ {
				fv, e := strconv.ParseFloat(unsafeString(args[j]), 32)
				if e != nil {
					parseErr = fmt.Sprintf("invalid float %q", unsafeString(args[j]))
					break filterParse
				}
				query = append(query, float32(fv))
			}
			break filterParse
		default:
			parseErr = fmt.Sprintf("unexpected token %q (want EQ|RANGE|VEC)", unsafeString(args[i]))
			break filterParse
		}
	}
	if parseErr != "" {
		buf.WriteError("ERR " + parseErr)
		return
	}
	if len(query) == 0 {
		buf.WriteError("ERR missing VEC query vector")
		return
	}
	monitoring.VectorSearchTotal.Inc()
	results, err := c.vec.SearchFilter(query, K, f)
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
	}
}

// cmdVSimInfo — VSIM.INFO: число векторов, размерность, уровни; сначала сбрасывает дельту.
func (c *commands) cmdVSimInfo(cs *server.ConnState, _ [][]byte) {
	buf := cs.Buf
	// FlushDeltaSync: гарантируем, что к моменту чтения состояния все вставленные
	// векторы перенесены из дельты в сегменты. Иначе при burst-write Info() видит
	// только частичные данные (компакция в полёте), а последующий Search работает
	// по неполному индексу. Бенчмарки (ann_bench) зовут VSIM.INFO как sync-точку.
	c.vec.FlushDeltaSync()
	count, dim, maxLevel := c.vec.Info()
	info := fmt.Sprintf("vectors:%d dimension:%d max_level:%d", count, dim, maxLevel)
	buf.WriteBulkString(info)
}

// cmdVSimSearchFilter — VSIM.SEARCHFILTER: поиск ближайших векторов с фильтрацией
// по метаданным в KV.
//
// Формат:  VSIM.SEARCHFILTER <K> <filter_field> <filter_value> <v1> <v2> ... <vN>
//
// Для каждого вектора с ключом X проверяется:
//
//	GET "filter_field:X" == filter_value
//
// Пример: если вектор имеет ключ "product:123", а метаданные хранятся как
//
//	SET "category:product:123" "electronics"
//
// то команда:
//
//	VSIM.SEARCHFILTER 10 category electronics 0.1 0.2 0.3 ...
//
// найдёт 10 ближайших векторов только из категории "electronics".
//
// Альтернативный режим с PREFIX:
//
//	VSIM.SEARCHFILTER <K> PREFIX <prefix> <v1> <v2> ... <vN>
//
// Фильтрует по префиксу ключа вектора. Например:
//
//	VSIM.SEARCHFILTER 10 PREFIX product: 0.1 0.2 0.3 ...
//
// найдёт только векторы, чей ключ начинается с "product:".
func (c *commands) cmdVSimSearchFilter(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 4 {
		buf.WriteError("ERR usage: VSIM.SEARCHFILTER <K> <filter_field> <filter_value> <v1> <v2> ... <vN>  or  VSIM.SEARCHFILTER <K> PREFIX <prefix> <v1> <v2> ... <vN>")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	filterField := string(args[1])
	filterValue := string(args[2])
	vecArgs := args[3:]

	if len(vecArgs) == 0 {
		buf.WriteError("ERR no vector components provided")
		return
	}

	query := make([]float32, len(vecArgs))
	for i, a := range vecArgs {
		f, err := strconv.ParseFloat(unsafeString(a), 32)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i+3, unsafeString(a)))
			return
		}
		query[i] = float32(f)
	}

	var filterFn func(string) bool

	if strings.ToUpper(filterField) == "PREFIX" {
		// Режим PREFIX: фильтрация по префиксу ключа вектора.
		prefix := filterValue
		filterFn = func(key string) bool {
			return strings.HasPrefix(key, prefix)
		}
	} else {
		// Режим KV: проверяем метаданные в KV Store.
		// Для вектора с ключом X ищем: GET "field:X" == value
		field := filterField
		value := filterValue
		filterFn = func(key string) bool {
			metaKey := field + ":" + key
			val, ok := c.kv.Get(metaKey)
			return ok && string(val) == value
		}
	}

	monitoring.VectorSearchTotal.Inc()
	searchStart := time.Now()
	results, err := c.vec.SearchFiltered(query, K, filterFn, nil)
	monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
	}
}

// cmdVSimSearchRange — VSIM.SEARCHRANGE: комбинированный поиск — вектор и
// диапазон score sorted set.
//
// VSIM.SEARCHRANGE <K> <setName> <minScore> <maxScore> <v1> <v2> ... <vN>
//
// Поток:
//  1. B+Tree RangeSearch → множество допустимых member'ов (O(log n + k))
//  2. HNSW SearchFiltered с filterFn = "member в множестве" → K лучших
//
// Без B+Tree пришлось бы делать N вызовов GET для каждого кандидата HNSW.
func (c *commands) cmdVSimSearchRange(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 5 {
		buf.WriteError("ERR usage: VSIM.SEARCHRANGE <K> <setName> <minScore> <maxScore> <v1> <v2> ... <vN>")
		return
	}
	K, err := strconv.Atoi(unsafeString(args[0]))
	if err != nil || K <= 0 || K > vector.MaxSearchK {
		buf.WriteError("ERR invalid K (must be 1..100000)")
		return
	}
	setName := string(args[1])
	minScore, err := strconv.ParseFloat(unsafeString(args[2]), 64)
	if err != nil || math.IsNaN(minScore) {
		buf.WriteError("ERR invalid minScore")
		return
	}
	maxScore, err := strconv.ParseFloat(unsafeString(args[3]), 64)
	if err != nil || math.IsNaN(maxScore) {
		buf.WriteError("ERR invalid maxScore")
		return
	}
	vecArgs := args[4:]
	if len(vecArgs) == 0 {
		buf.WriteError("ERR no vector components provided")
		return
	}
	query := make([]float32, len(vecArgs))
	for i, a := range vecArgs {
		f, err := strconv.ParseFloat(unsafeString(a), 32)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i+4, unsafeString(a)))
			return
		}
		query[i] = float32(f)
	}

	// Шаг 1: B+-дерево → множество member в диапазоне. Строки не копируются:
	// map держит те же строки, что и дерево. Сравнение точное, без прежних
	// FNV-хешей и их коллизий.
	allowed := c.zsets.MembersInRange(setName, minScore, maxScore)
	if allowed == nil {
		// Пустой sorted set или нет элементов в диапазоне
		buf.WriteArrayHeader(0)
		return
	}

	// Шаг 2: HNSW SearchFiltered с фильтром «member в множестве».
	monitoring.VectorSearchTotal.Inc()
	searchStart := time.Now()
	results, err := c.vec.SearchFiltered(query, K, func(key string) bool {
		_, ok := allowed[key]
		return ok
	}, nil)
	monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
	if err != nil {
		buf.WriteError(fmt.Sprintf("ERR %v", err))
		return
	}
	buf.WriteArrayHeader(len(results) * 2)
	for _, r := range results {
		buf.WriteBulkString(r.Key)
		buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
	}
}

// parseAttrFilter парсит хвост [EQ <attr> <val>]... [RANGE <attr> <lo> <hi>]...
// начиная с args[start] (синтаксис VSIM.FILTER; тот же разбор у SEARCHTEXT и
// HYBRID, чтобы фильтры не разъехались). Останавливается на первом токене, не
// являющемся EQ/RANGE (например VEC), и возвращает фильтр, индекс этого
// токена и текст ошибки (""=ок). Без токенов возвращает нулевой Filter
// (f.empty()=true — команды сохраняют старое поведение бит-в-бит).
func parseAttrFilter(args [][]byte, start int) (vector.Filter, int, string) {
	var f vector.Filter
	i := start
	for i < len(args) {
		switch strings.ToUpper(string(args[i])) {
		case "EQ":
			if i+2 >= len(args) {
				return f, i, "EQ requires <attr> <value>"
			}
			if f.Eq == nil {
				f.Eq = map[string]string{}
			}
			f.Eq[string(args[i+1])] = string(args[i+2])
			i += 3
		case "RANGE":
			if i+3 >= len(args) {
				return f, i, "RANGE requires <attr> <lo> <hi>"
			}
			lo, e1 := strconv.ParseFloat(unsafeString(args[i+2]), 64)
			hi, e2 := strconv.ParseFloat(unsafeString(args[i+3]), 64)
			if e1 != nil || e2 != nil {
				return f, i, "RANGE lo/hi not floats"
			}
			if f.Range == nil {
				f.Range = map[string][2]float64{}
			}
			f.Range[string(args[i+1])] = [2]float64{lo, hi}
			i += 4
		default:
			return f, i, ""
		}
	}
	return f, i, ""
}
