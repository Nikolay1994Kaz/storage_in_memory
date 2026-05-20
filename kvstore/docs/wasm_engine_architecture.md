# WASM Compute Engine Architecture / Архитектура Вычислительного WASM-Движка

*This document explains the core mechanics of the Zero-Allocation WASM Reactor engine, referencing exact functions and code paths. / Этот документ описывает базовые механики Zero-Allocation WASM движка с указанием конкретных функций.*

---

## 🇷🇺 Русский (Russian)

### 1. Два типа Runtime (Виртуальных Машин)
В движке инициализируются **два изолированных `wazero.Runtime`** для разных задач:

1. **Global Runtime** (`internal/compute/runtime.go -> NewEngine()`): 
   * **Настройка**: Создается через `wazero.NewRuntimeConfig().WithCloseOnContextDone(true)`.
   * **Поведение**: Используется для разовых задач. Если родительский контекст отменяется, `wazero` мгновенно убивает WASM-модуль. Это защищает от зависаний.
2. **Worker-Local Runtime** (`internal/compute/worker_slot.go -> NewWorkerLocalEngine()`): 
   * **Настройка**: Создается **БЕЗ** `WithCloseOnContextDone`.
   * **Поведение**: Используется для бессмертных высоконагруженных модулей. Он игнорирует отмены контекстов, чтобы модули-обработчики не убивались при тайм-аутах запросов клиентов.

### 2. Типы модулей (Tiers) и Инстанцирование
Система распределяет модули по уровням в функции `WarmUpFromBytes`:

* **Tier 2 (Command)**: Работает в Global Runtime. Обычный бинарник со `_start`. На каждый вызов происходит `InstantiateModule`, выполнение и полное уничтожение.
* **Tier 0 (Pinned Reactor)** и **Tier 1 (Budget Reactor)**:
  * Загружаются в Worker-Local Runtime.
  * Скомпилированы с флагом `-buildmode=c-shared`. 
  * При создании (`createSlot()`) мы передаем `WithStartFunctions()`, чтобы заблокировать вызов `_start` (иначе `proc_exit` убьет модуль).
  * Движок вручную проверяет наличие экспорта `_initialize` (через `IsReactorModule`) и вызывает его **ровно один раз**: `initFn.Call(ctx)`. Инстанс остается живым навсегда.
  * *Отличие Tier 1*: В `worker_slot.go` задается `memoryBudget` (например, 128 страниц). В конце каждого вызова `Exec()` проверяется `slot.memory.Size()`. Если бюджет превышен, вызывается `wle.recycleSlot(slot)` — старый инстанс удаляется, создается чистый из `wazero.CompiledModule`.

### 3. Архитектура Worker-Local (Слоты)
Для модулей Tier 0 и Tier 1 используется **Thread-Local Storage (TLS)**:
* Функция `WarmUpFromBytes()` создает массив из N слотов (по числу ядер, e.g. 12).
* Каждый слот кэширует указатели на функции: `funcs[exportName] = instance.ExportedFunction(exportName)`, чтобы избежать накладных расходов при поиске по строке на горячем пути.
* Воркер №5 всегда работает только со `slots[5]`. Никаких мьютексов (`sync.RWMutex`), Data Races физически невозможны.

### 4. Полный Флоу (Zero-Allocation Pipeline)
Весь горячий путь проходит в функции `Exec()` без выделения памяти (malloc), работая через жестко зафиксированные `Offsets` (адреса) в разделяемой памяти:

1. **Start (Go)**: В `Exec()` воркер берет свой `slot`. Ключ (e.g. `tx:1001`) записывается напрямую в память: `slot.memory.Write(InputOffset, key)`. Воркер вызывает функцию: `fn.Call(ctx, InputOffset, len)`.
2. **Переход в WASM**: WASM-модуль (например, `fraud_scorer.wasm`) читает свой инпут. Понимая, что нужна транзакция из БД, он вызывает системный импорт `env.kv_get`.
3. **Host-Функция (Go)**: Управление перехватывается в `host_functions.go`.
   * Мы используем `api.GoModuleFunc` вместо `wazero.WithFunc()`. Это позволяет нам читать аргументы напрямую из регистров виртуальной машины: `keyPtr := uint32(stack[0])`. (Это **полностью исключает пакет `reflect`** и спасает от Cache Line Bouncing на 12 ядрах).
   * Go вызывает `e.StoreGet()`, получает JSON из реальной БД.
   * Go пишет этот JSON напрямую в память спящего WASM: `m.Memory().Write(ValueOffset, val)`.
   * Go возвращает длину ответа в регистр: `stack[0] = uint64(len(val))`.
4. **Бизнес-логика (WASM)**: WASM забирает JSON из `ValueOffset`, принимает решение (e.g., `"blocked"`) и пишет итоговый вердикт в `OutputOffset` (5120). Возвращает длину вердикта.
5. **End (Go)**: `fn.Call()` в `Exec()` завершается. Воркер читает итоговый результат из `OutputOffset` и отдает его клиенту. Вызов завершен за ~240 наносекунд (4.5M RPS).

---

## 🇬🇧 English

### 1. Two Types of Runtimes (Virtual Machines)
The engine initializes **two isolated `wazero.Runtime` instances** for different tasks:

1. **Global Runtime** (`internal/compute/runtime.go -> NewEngine()`): 
   * **Setup**: Created with `wazero.NewRuntimeConfig().WithCloseOnContextDone(true)`.
   * **Behavior**: Used for one-off tasks. If the parent context is cancelled, `wazero` instantly kills the WASM module. This prevents infinite loops.
2. **Worker-Local Runtime** (`internal/compute/worker_slot.go -> NewWorkerLocalEngine()`): 
   * **Setup**: Created **WITHOUT** `WithCloseOnContextDone`.
   * **Behavior**: Used for immortal, high-throughput modules. It ignores context cancellations so that Reactor modules aren't accidentally killed during single request timeouts.

### 2. Module Tiers & Instantiation
The system assigns modules to tiers in the `WarmUpFromBytes` function:

* **Tier 2 (Command)**: Runs in the Global Runtime. Standard binary with `_start`. Instantiated, executed, and destroyed on every call.
* **Tier 0 (Pinned Reactor)** & **Tier 1 (Budget Reactor)**:
  * Loaded into the Worker-Local Runtime.
  * Compiled with `-buildmode=c-shared`.
  * During creation (`createSlot()`), we pass `WithStartFunctions()` to block the implicit `_start` call (otherwise `proc_exit` kills the module).
  * The engine manually checks for the `_initialize` export (via `IsReactorModule`) and calls it **exactly once**: `initFn.Call(ctx)`. The instance stays alive forever.
  * *Tier 1 Difference*: `worker_slot.go` assigns a `memoryBudget` (e.g., 128 pages). At the end of `Exec()`, it checks `slot.memory.Size()`. If exceeded, `wle.recycleSlot(slot)` destroys the bloated instance and respawns a clean one from the cached `wazero.CompiledModule`.

### 3. Worker-Local Architecture (Slots)
Tier 0 and Tier 1 use the **Thread-Local Storage (TLS)** pattern:
* `WarmUpFromBytes()` creates an array of N slots (matching CPU cores, e.g., 12).
* Each slot caches function pointers: `funcs[exportName] = instance.ExportedFunction(exportName)` to avoid expensive string lookups on the hot path.
* Worker #5 always uses `slots[5]`. No mutexes (`sync.RWMutex`) are used, making Data Races physically impossible.

### 4. Full Zero-Allocation Pipeline
The entire hot path runs in `Exec()` without dynamic memory allocation (malloc), using fixed `Offsets` (addresses) in shared memory:

1. **Start (Go)**: In `Exec()`, the worker grabs its `slot`. The key (e.g., `tx:1001`) is written directly: `slot.memory.Write(InputOffset, key)`. The worker invokes: `fn.Call(ctx, InputOffset, len)`.
2. **WASM Transition**: The WASM module (e.g., `fraud_scorer.wasm`) reads its input. Realizing it needs DB data, it invokes the system import `env.kv_get`.
3. **Host-Function (Go)**: Control is intercepted in `host_functions.go`.
   * We use `api.GoModuleFunc` instead of `wazero.WithFunc()`. This allows reading arguments directly from the VM registers: `keyPtr := uint32(stack[0])`. (This **completely eliminates the `reflect` package** and prevents Cache Line Bouncing on 12 cores).
   * Go calls `e.StoreGet()` to fetch the real DB JSON.
   * Go writes this JSON directly into the sleeping WASM's memory: `m.Memory().Write(ValueOffset, val)`.
   * Go returns the response length into the register: `stack[0] = uint64(len(val))`.
4. **Business Logic (WASM)**: WASM fetches the JSON from `ValueOffset`, makes a decision (e.g., `"blocked"`), and writes the verdict to `OutputOffset` (5120). Returns the verdict length.
5. **End (Go)**: `fn.Call()` in `Exec()` finishes. The worker reads the final result from `OutputOffset` and serves it to the client. The entire flow completes in ~240 nanoseconds (4.5M RPS).
