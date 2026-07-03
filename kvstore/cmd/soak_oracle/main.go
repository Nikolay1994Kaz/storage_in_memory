// soak_oracle — детерминированный durability+correctness oracle для soak-теста.
//
// Идея: soak-нагрузка (stress_test) меряет ЖИВУЧЕСТЬ и ДРЕЙФ, но слепа к
// КОРРЕКТНОСТИ данных после рестарта. Этот инструмент закрывает пробел дёшево:
//
//	seed   — засевает N известных KV+векторов (namespace oracle:*), затем удаляет
//	         детерминированное подмножество (i%5==0). Namespace не пересекается с
//	         ключами stress_test (key:*, vec:*, benchset), поэтому нагрузка их не
//	         трогает — переживают только через durability/merge.
//	verify — после рестарта сервера проверяет:
//	           живые   (i%5!=0): GET==val И VSIM.SEARCH находит вектор;
//	           удалён. (i%5==0): GET==nil И вектора НЕТ в выдаче SEARCH.
//	         Воскрешение удалённого или потеря живого → exit 1 (сборка красная).
//
// Ловит ровно durability-класс, что чинили 03.07: watermark-гонка (потеря на
// рестарте), OpDel/OpExpire-гейтинг и tombstone-воскрешение.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"time"

	"kvstore/kvstore/internal/protocol"
)

const (
	dim      = 128 // ОБЯЗАН совпадать с dim в stress_test (иначе VSIM.ADD → dim mismatch)
	kvNS     = "oracle:kv:"
	vecNS    = "oracle:vec:"
	delEvery = 5 // удаляем каждый 5-й ключ (i%5==0)
)

func main() {
	addr := flag.String("addr", "localhost:6380", "адрес сервера")
	mode := flag.String("mode", "", "seed | verify")
	n := flag.Int("n", 200, "число контрольных ключей")
	rngSeed := flag.Int64("rngseed", 42, "seed для детерминированных векторов")
	flag.Parse()

	if *mode != "seed" && *mode != "verify" {
		fmt.Fprintln(os.Stderr, "usage: soak_oracle -mode seed|verify [-addr] [-n] [-rngseed]")
		os.Exit(2)
	}

	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *addr, err)
		os.Exit(2)
	}
	defer conn.Close()
	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)

	switch *mode {
	case "seed":
		if err := seed(w, r, *n, *rngSeed); err != nil {
			fmt.Fprintf(os.Stderr, "seed: %v\n", err)
			os.Exit(2)
		}
	case "verify":
		if err := verify(w, r, *n, *rngSeed); err != nil {
			fmt.Fprintf(os.Stderr, "verify: %v\n", err)
			os.Exit(1)
		}
	}
}

// vecFor — детерминированный вектор для ключа i (одинаков в seed и verify).
func vecFor(rngSeed int64, i int) []string {
	rng := rand.New(rand.NewSource(rngSeed + int64(i)))
	out := make([]string, dim)
	for d := 0; d < dim; d++ {
		out[d] = strconv.FormatFloat(rng.NormFloat64(), 'f', 6, 32)
	}
	return out
}

func cmd(w *protocol.Writer, r *protocol.Reader, args ...string) (protocol.Value, error) {
	arr := make([]protocol.Value, len(args))
	for i, a := range args {
		arr[i] = protocol.Value{Typ: '$', Str: a}
	}
	if err := w.Write(protocol.Value{Typ: '*', Array: arr}); err != nil {
		return protocol.Value{}, err
	}
	if err := w.Flush(); err != nil { // bufio.Writer: без Flush команда не уйдёт → Read зависнет
		return protocol.Value{}, err
	}
	return r.Read()
}

func seed(w *protocol.Writer, r *protocol.Reader, n int, rngSeed int64) error {
	for i := 0; i < n; i++ {
		if _, err := cmd(w, r, "SET", kvNS+strconv.Itoa(i), "val:"+strconv.Itoa(i)); err != nil {
			return err
		}
		if _, err := cmd(w, r, append([]string{"VSIM.ADD", vecNS + strconv.Itoa(i)}, vecFor(rngSeed, i)...)...); err != nil {
			return err
		}
	}
	// Детерминированное подмножество удаляем.
	deleted := 0
	for i := 0; i < n; i++ {
		if i%delEvery != 0 {
			continue
		}
		if _, err := cmd(w, r, "DEL", kvNS+strconv.Itoa(i)); err != nil {
			return err
		}
		if _, err := cmd(w, r, "VSIM.DEL", vecNS+strconv.Itoa(i)); err != nil {
			return err
		}
		deleted++
	}
	// VSIM.INFO триггерит FlushDeltaSync — векторы уезжают в сегменты до нагрузки.
	if _, err := cmd(w, r, "VSIM.INFO"); err != nil {
		return err
	}
	fmt.Printf("[oracle seed] n=%d live=%d deleted=%d dim=%d\n", n, n-deleted, deleted, dim)
	return nil
}

// searchHasKey возвращает true, если key присутствует в выдаче VSIM.SEARCH по
// вектору i. Ответ — массив [key, dist, key, dist, ...]; ключи на чётных позициях.
func searchHasKey(w *protocol.Writer, r *protocol.Reader, key string, i int, rs int64) (bool, error) {
	args := append([]string{"VSIM.SEARCH", "10"}, vecFor(rs, i)...)
	v, err := cmd(w, r, args...)
	if err != nil {
		return false, err
	}
	if v.Typ != '*' {
		return false, fmt.Errorf("VSIM.SEARCH: ждали массив, got typ=%q str=%q", v.Typ, v.Str)
	}
	for j := 0; j < len(v.Array); j += 2 {
		if v.Array[j].Str == key {
			return true, nil
		}
	}
	return false, nil
}

func verify(w *protocol.Writer, r *protocol.Reader, n int, rngSeed int64) error {
	var kvLost, kvResurrect, vecLost, vecResurrect, badVal int
	for i := 0; i < n; i++ {
		kvKey := kvNS + strconv.Itoa(i)
		vecKey := vecNS + strconv.Itoa(i)
		deleted := i%delEvery == 0

		g, err := cmd(w, r, "GET", kvKey)
		if err != nil {
			return err
		}
		isNil := g.Typ == '$' && g.Num == -1
		hasVec, err := searchHasKey(w, r, vecKey, i, rngSeed)
		if err != nil {
			return err
		}

		if deleted {
			if !isNil {
				kvResurrect++ // удалённый KV воскрес
			}
			if hasVec {
				vecResurrect++ // удалённый вектор воскрес
			}
		} else {
			if isNil {
				kvLost++ // живой KV потерян
			} else if g.Str != "val:"+strconv.Itoa(i) {
				badVal++ // живой KV с неверным значением
			}
			if !hasVec {
				vecLost++ // живой вектор потерян
			}
		}
	}

	bugs := kvLost + kvResurrect + vecLost + vecResurrect + badVal
	fmt.Printf("[oracle verify] n=%d | KV lost=%d resurrect=%d badval=%d | VEC lost=%d resurrect=%d\n",
		n, kvLost, kvResurrect, badVal, vecLost, vecResurrect)
	if bugs > 0 {
		return fmt.Errorf("ORACLE FAIL: %d нарушений durability/tombstone (потеря/воскрешение/порча)", bugs)
	}
	fmt.Println("[oracle verify] OK — данные пережили рестарт, удалённые не воскресли")
	return nil
}
