# WAL Benchmarks

В этом документе сохранены результаты производительности подсистемы Write-Ahead Log (WAL) in-memory хранилища. Замеры показывают эффективность батчинга (`BatchWAL`) и zero-allocation кодирования на горячих путях.

## Результаты

**CPU:** Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz
**OS/Arch:** linux/amd64

```text
BenchmarkWAL_Write-12                   18587944         348.1 ns/op          40 B/op          2 allocs/op
BenchmarkBatchWAL_Write-12              27400725         137.8 ns/op           0 B/op          0 allocs/op
BenchmarkEncode-12                     347993520          9.779 ns/op          0 B/op          0 allocs/op
BenchmarkReadEntries-12                      487       6504561 ns/op     2722202 B/op      40025 allocs/op
BenchmarkBatchWAL_Write_Parallel-12      7355222         621.9 ns/op           0 B/op          0 allocs/op
BenchmarkBatchWAL_FlushBatch-12           173514         21007 ns/op           0 B/op          0 allocs/op
```

## Анализ показателей

1. **`BenchmarkBatchWAL_Write` (137.8 ns/op | 0 B/op | 0 allocs/op)**
   Основной горячий путь для клиентов (Client Hot Path). Использование неблокирующего канала позволяет избежать взятия мьютекса (`mu.Lock`) на каждую запись. Результат: **0 аллокаций памяти**, высочайшая пропускная способность (~7 млн операций в секунду на ядро).

2. **`BenchmarkBatchWAL_FlushBatch` (21007 ns/op | 0 allocs/op)**
   Чистое время работы фонового `flusher`-а на один полный батч из 256 записей. Включает подсчет `CRC32`, сериализацию всех ключей и значений в переиспользуемый буфер `encodeBuf` и один вызов `file.Write()`. 
   `21007 / 256 = ~82 наносекунды` на одну запись. Идеальная zero-alloc сериализация.

3. **`BenchmarkWAL_Write` (348.1 ns/op | 40 B/op | 2 allocs/op)**
   Запись одной записи "в лоб" с захватом мьютекса (Cold Path, используется в `snapshot.go`). В 2.5 раза медленнее батчера даже в 1 поток. Вызывает 2 аллокации (преимущественно на `interface{}` в `binary.Write`, если бы он использовался, но здесь аллокации вызваны `encodeEntry` и вызовом метода интерфейса `io.Writer`).

4. **`BenchmarkEncode` (9.77 ns/op)**
   Ультра-быстрое кодирование байтов и подсчет CRC32 для одной записи в памяти без I/O.

5. **`BenchmarkReadEntries` (6.5 ms / 10 000 entries)**
   Скорость восстановления базы при "холодном старте". Аллокации здесь (40025) абсолютно нормальны, так как база физически обязана выделить память в RAM для каждого прочитанного ключа и значения, чтобы положить их в In-Memory хэш-таблицу. Скорость восстановления: **~1.5 млн записей в секунду**.
