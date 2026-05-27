# История бенчмарков (WASM Compute Engine)

Этот документ хранит исторические данные о производительности WASM-движка базы данных KVStore на разных этапах оптимизации.

## Этап 1: Исходная реализация (Baseline MVP)
**Состояние:** Использовался `fmt.Sprintf` и `time.Now().UnixNano()` для генерации имен инстансов. `wazero` каждый раз инстанцировался с нуля. Логирование было включено.

```text
goos: linux
goarch: amd64
pkg: kvstore/kvstore/internal/compute
cpu: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz

BenchmarkRealWasm_FraudScorer-12              100    14176381 ns/op    2807578 B/op    53241 allocs/op
BenchmarkRealWasm_FraudScorer_SmallTx-12       80    13761859 ns/op    2808122 B/op    53242 allocs/op
BenchmarkRealWasm_FraudScorer_BlockedTx-12     80    12821470 ns/op    2807844 B/op    53240 allocs/op
```

---

## Этап 2: Гигиена и удаление `fmt.Sprintf`
**Состояние:** Логи подавлены. `fmt.Sprintf` заменен на lock-free `atomic.Uint64` + `strconv`. Устранен налог на системный вызов `time.Now()`. Добавлен параллельный бенчмарк.

```text
goos: linux
goarch: amd64
pkg: kvstore/kvstore/internal/compute
cpu: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz

BenchmarkRealWasm_FraudScorer-12              262     4896741 ns/op       0.01 MB/s    2805113 B/op    53237 allocs/op
BenchmarkRealWasm_FraudScorer_SmallTx-12      236     4975837 ns/op       0.01 MB/s    2805220 B/op    53237 allocs/op
BenchmarkRealWasm_FraudScorer_BlockedTx-12    240     4850000 ns/op       0.01 MB/s    2805000 B/op    53237 allocs/op
BenchmarkRealWasm_FraudScorer_Parallel-12     408     3063361 ns/op       0.01 MB/s    1095430 B/op    13906 allocs/op
```
*(Ускорение почти в 3 раза, -4 лишние аллокации на операцию)*

---

## Этап 3: Instance Pooling (План)
**Ожидание:** Внедрение `sync.Pool` для переиспользования инстансов `wazero`.
*... результаты будут добавлены после внедрения пула ...*
