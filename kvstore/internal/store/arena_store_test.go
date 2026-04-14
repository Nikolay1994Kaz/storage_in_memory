package store

import (
	"fmt"
	"sync"
	"testing"
)

// ─────────────────────────────────────────────────────────────
// Тест 1: Базовые операции (Set, Get, Del)
//
// Table-Driven тест — стандарт в Go.
// Описываем набор сценариев в виде таблицы (слайс структур),
// а цикл прогоняет каждый. Плюс: чтобы добавить новый кейс,
// просто дописываешь строчку в таблицу.
// ─────────────────────────────────────────────────────────────
func TestArenaStore_BasicCRUD(t *testing.T) {
	s := NewArenaStore()

	// --- SET + GET ---
	tests := []struct {
		name  string // имя подтеста (для отладки)
		key   string
		value string
	}{
		{"simple key", "name", "Nikolay"},
		{"key with colon", "user:1", `{"id":1,"name":"test"}`},
		{"empty value", "empty", ""},
		{"unicode key", "ключ", "значение"},
		{"long value", "big", string(make([]byte, 10000))},
	}

	for _, tt := range tests {
		t.Run("Set_Get/"+tt.name, func(t *testing.T) {
			s.Set(tt.key, []byte(tt.value))

			got, ok := s.Get(tt.key)
			if !ok {
				t.Fatalf("Get(%q): ключ не найден", tt.key)
			}
			if string(got) != tt.value {
				t.Fatalf("Get(%q) = %q, хотели %q", tt.key, string(got), tt.value)
			}
		})
	}

	// --- GET несуществующего ключа ---
	t.Run("Get_NonExistent", func(t *testing.T) {
		_, ok := s.Get("несуществующий_ключ")
		if ok {
			t.Fatal("Get вернул ok=true для несуществующего ключа")
		}
	})

	// --- DEL ---
	t.Run("Del", func(t *testing.T) {
		s.Set("to_delete", []byte("bye"))

		deleted := s.Del("to_delete")
		if !deleted {
			t.Fatal("Del вернул false для существующего ключа")
		}

		_, ok := s.Get("to_delete")
		if ok {
			t.Fatal("Ключ всё ещё существует после Del")
		}
	})

	// --- DEL несуществующего ключа ---
	t.Run("Del_NonExistent", func(t *testing.T) {
		deleted := s.Del("never_existed")
		if deleted {
			t.Fatal("Del вернул true для несуществующего ключа")
		}
	})
}

// ─────────────────────────────────────────────────────────────
// Тест 2: Перезапись значения
//
// ArenaStore при SET не удаляет старые данные из арены —
// он просто дописывает новые и ОБНОВЛЯЕТ индекс (offset).
// Старые байты остаются мусором в буфере (append-only).
// Этот тест проверяет, что Get вернёт именно ПОСЛЕДНЕЕ
// записанное значение, а не одно из старых.
// ─────────────────────────────────────────────────────────────
func TestArenaStore_Overwrite(t *testing.T) {
	s := NewArenaStore()

	s.Set("counter", []byte("1"))
	s.Set("counter", []byte("2"))
	s.Set("counter", []byte("3"))

	got, ok := s.Get("counter")
	if !ok {
		t.Fatal("Ключ не найден после перезаписи")
	}
	if string(got) != "3" {
		t.Fatalf("Перезапись не сработала: got %q, want %q", string(got), "3")
	}
}

// ─────────────────────────────────────────────────────────────
// Тест 3: Len (счётчик ключей)
//
// Проверяем что Len() корректно считает после:
//   - добавления ключей
//   - перезаписи (НЕ должна увеличивать счётчик)
//   - удаления
//
// ─────────────────────────────────────────────────────────────
func TestArenaStore_Len(t *testing.T) {
	s := NewArenaStore()

	if s.Len() != 0 {
		t.Fatalf("Пустой store: Len() = %d, хотели 0", s.Len())
	}

	s.Set("a", []byte("1"))
	s.Set("b", []byte("2"))
	s.Set("c", []byte("3"))

	if s.Len() != 3 {
		t.Fatalf("После 3 SET: Len() = %d, хотели 3", s.Len())
	}

	// Перезапись НЕ увеличивает счётчик
	s.Set("a", []byte("updated"))
	if s.Len() != 3 {
		t.Fatalf("После перезаписи: Len() = %d, хотели 3", s.Len())
	}

	s.Del("b")
	if s.Len() != 2 {
		t.Fatalf("После Del: Len() = %d, хотели 2", s.Len())
	}
}

// ─────────────────────────────────────────────────────────────
// Тест 4: ForEach (обход всех ключей)
//
// Проверяем, что ForEach обходит ВСЕ ключи ровно один раз
// и значения совпадают.
// ─────────────────────────────────────────────────────────────
func TestArenaStore_ForEach(t *testing.T) {
	s := NewArenaStore()

	expected := map[string]string{
		"key1": "val1",
		"key2": "val2",
		"key3": "val3",
	}

	for k, v := range expected {
		s.Set(k, []byte(v))
	}

	visited := make(map[string]string)
	s.ForEach(func(key string, value []byte) {
		visited[key] = string(value)
	})

	if len(visited) != len(expected) {
		t.Fatalf("ForEach обошёл %d ключей, хотели %d", len(visited), len(expected))
	}

	for k, want := range expected {
		got, ok := visited[k]
		if !ok {
			t.Fatalf("ForEach пропустил ключ %q", k)
		}
		if got != want {
			t.Fatalf("ForEach: ключ %q = %q, хотели %q", k, got, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// Тест 5: Гонки данных (Race Condition)
//
// САМЫЙ ВАЖНЫЙ ТЕСТ.
//
// 100 горутин одновременно бьют по 50 ключам:
//   - 60% SET (запись)
//   - 30% GET (чтение)
//   - 10% DEL (удаление)
//
// Если ArenaStore не потокобезопасен — Go Race Detector
// поймает гонку и тест упадёт с паникой.
//
// Запускать ОБЯЗАТЕЛЬНО с флагом -race:
//
//	go test -race ./internal/store/ -run ConcurrentAccess
//
// ─────────────────────────────────────────────────────────────
func TestArenaStore_ConcurrentAccess(t *testing.T) {
	s := NewArenaStore()

	const (
		goroutines = 100
		iterations = 1000
		keySpace   = 50
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				key := fmt.Sprintf("key_%d", i%keySpace)
				val := fmt.Sprintf("goroutine_%d_iter_%d", id, i)

				switch i % 10 {
				case 0:
					s.Del(key)
				case 1, 2, 3:
					s.Get(key)
				default:
					s.Set(key, []byte(val))
				}
			}
		}(g)
	}

	wg.Wait()

	count := s.Len()
	t.Logf("После нагрузки: %d ключей в store (из %d)", count, keySpace)

	if count > keySpace {
		t.Fatalf("Len() = %d > keySpace %d: дубли!", count, keySpace)
	}

	// Все оставшиеся ключи должны читаться без паники
	s.ForEach(func(key string, value []byte) {
		if len(key) == 0 {
			t.Fatal("Пустой ключ в ForEach — повреждение данных!")
		}
	})
}
