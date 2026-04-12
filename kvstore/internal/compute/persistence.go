package compute

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// wasmDir — поддиректория внутри data/ для хранения модулей и триггеров.
const wasmDir = "wasm"

// triggerConfig — структура для сериализации одного триггера в JSON.
// Мы не можем сохранить *Trigger напрямую, потому что его поле ID
// генерируется автоматически. Нам нужны только данные для восстановления.
type triggerConfig struct {
	Event      string `json:"event"`
	Pattern    string `json:"pattern"`
	ModuleName string `json:"module"`
	FuncName   string `json:"func"`
}

// WasmDir возвращает полный путь к директории хранения WASM.
// dataDir — это корневая директория данных сервера (обычно "./data").
func WasmDir(dataDir string) string {
	return filepath.Join(dataDir, wasmDir)
}

// SaveModule сохраняет байткод WASM-модуля на диск.
// Вызывается из main.go после успешного engine.LoadModule().
//
// Логика:
//  1. Создаём директорию data/wasm/ если её нет
//  2. Записываем байты в файл module_name.wasm
func SaveModule(dataDir, name string, wasmBytes []byte) error {
	dir := WasmDir(dataDir)

	// os.MkdirAll — создаёт всю цепочку директорий, если не существует.
	// Аналог `mkdir -p` в bash. Если директория уже есть — ничего не делает.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create wasm dir: %w", err)
	}

	path := filepath.Join(dir, name+".wasm")

	// os.WriteFile — атомарно записывает весь файл.
	// 0644 = владелец может читать/писать, остальные только читать.
	if err := os.WriteFile(path, wasmBytes, 0644); err != nil {
		return fmt.Errorf("save module %s: %w", name, err)
	}

	log.Printf("[wasm] Module '%s' saved to disk (%d bytes)", name, len(wasmBytes))
	return nil
}

// DeleteModule удаляет файл модуля с диска.
// Вызывается из main.go при WASM.DROP.
func DeleteModule(dataDir, name string) {
	path := filepath.Join(WasmDir(dataDir), name+".wasm")
	os.Remove(path) // Игнорируем ошибку — файла может не быть
}

// SaveTriggers сериализует все триггеры в JSON и сохраняет на диск.
// Вызывается после каждого WASM.TRIGGER / WASM.UNTRIGGER.
//
// Почему JSON, а не бинарный формат?
// Триггеров обычно мало (5-20 штук), запись редкая (раз в час).
// JSON позволяет человеку руками прочитать/отредактировать конфигурацию.
func SaveTriggers(dataDir string, tm *TriggerManager) error {
	dir := WasmDir(dataDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create wasm dir: %w", err)
	}

	// Получаем снимок всех триггеров (под RLock внутри ListTriggers)
	all := tm.ListTriggers()

	// Конвертируем в сериализуемый формат
	configs := make([]triggerConfig, len(all))
	for i, t := range all {
		configs[i] = triggerConfig{
			Event:      string(t.Event),
			Pattern:    t.Pattern,
			ModuleName: t.ModuleName,
			FuncName:   t.FuncName,
		}
	}

	// json.MarshalIndent — как json.Marshal, но с красивым форматированием.
	// Второй аргумент "" — префикс (пустой), третий "  " — отступ (2 пробела).
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal triggers: %w", err)
	}

	path := filepath.Join(dir, "triggers.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("save triggers: %w", err)
	}

	log.Printf("[wasm] Saved %d triggers to disk", len(configs))
	return nil
}

// LoadAll восстанавливает состояние WASM при старте сервера.
// Читает все .wasm файлы из data/wasm/ и triggers.json.
//
// Порядок важен:
//  1. Сначала загружаем модули (они нужны триггерам)
//  2. Потом восстанавливаем триггеры (они ссылаются на модули по имени)
func LoadAll(dataDir string, engine *Engine, tm *TriggerManager) error {
	dir := WasmDir(dataDir)

	// Проверяем, существует ли директория. Если нет — первый запуск, ничего не делаем.
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil // Нет директории = нет сохранённых модулей. Это нормально.
	}

	// --- Шаг 1: Загружаем все .wasm файлы ---
	// filepath.Glob — ищет файлы по glob-паттерну (как ls *.wasm в bash).
	files, _ := filepath.Glob(filepath.Join(dir, "*.wasm"))
	moduleCount := 0

	for _, path := range files {
		// Извлекаем имя модуля из имени файла:
		// "/data/wasm/fraud_scorer.wasm" → "fraud_scorer"
		base := filepath.Base(path)               // "fraud_scorer.wasm"
		name := strings.TrimSuffix(base, ".wasm") // "fraud_scorer"

		wasmBytes, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[wasm] WARNING: cannot read %s: %v", path, err)
			continue // Пропускаем битый файл, не ломаем весь старт
		}

		if err := engine.LoadModule(name, wasmBytes); err != nil {
			log.Printf("[wasm] WARNING: cannot load module %s: %v", name, err)
			continue
		}
		moduleCount++
	}

	// --- Шаг 2: Восстанавливаем триггеры ---
	triggerPath := filepath.Join(dir, "triggers.json")
	triggerCount := 0

	data, err := os.ReadFile(triggerPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Нет файла триггеров — это нормально (модули есть, триггеры ещё не настроены)
			log.Printf("[wasm] Restored %d modules, 0 triggers", moduleCount)
			return nil
		}
		return fmt.Errorf("read triggers.json: %w", err)
	}

	var configs []triggerConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return fmt.Errorf("parse triggers.json: %w", err)
	}

	for _, cfg := range configs {
		tm.AddTrigger(TriggerEvent(cfg.Event), cfg.Pattern, cfg.ModuleName, cfg.FuncName)
		triggerCount++
	}

	log.Printf("[wasm] Restored %d modules, %d triggers", moduleCount, triggerCount)
	return nil
}
