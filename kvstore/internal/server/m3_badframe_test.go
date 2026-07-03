package server

import "testing"

// TestParse_BadArrayLength_ProtoErr — страж M3.
//
// Длина массива не число ("*abc"), но строка прочитана целиком. Раньше парсер
// возвращал nil как «неполные данные» → кадр застревал в буфере → wedge
// соединения (спасал только idle-реапер). Теперь — protoErr → handleConn рвёт.
func TestParse_BadArrayLength_ProtoErr(t *testing.T) {
	cb := newTestParser([]byte("*abc\r\n"))
	if got := cb.ParseCommand(); got != nil {
		t.Fatalf("ожидали nil на битой длине массива, got %v", got)
	}
	if !cb.ProtoErr() {
		t.Fatal("M3: ProtoErr не выставлен на '*abc' — соединение зависло бы (wedge)")
	}
}

// TestParse_BadBulkLength_ProtoErr — страж M3 для битой длины bulk ("$abc").
func TestParse_BadBulkLength_ProtoErr(t *testing.T) {
	cb := newTestParser([]byte("*1\r\n$abc\r\n"))
	if got := cb.ParseCommand(); got != nil {
		t.Fatalf("ожидали nil на битой длине bulk, got %v", got)
	}
	if !cb.ProtoErr() {
		t.Fatal("M3: ProtoErr не выставлен на '$abc' — соединение зависло бы (wedge)")
	}
}

// TestParse_EmptyInline_Consumed — страж M3 для пустой inline-строки.
//
// Одинокий "\r\n" раньше давал nil при УЖЕ сдвинутом rpos, ParseCommand
// откатывал rpos → строка перечитывалась вечно (wedge). Теперь пустая строка
// потребляется (непустой пустой срез → "ERR empty command"), а следующая
// валидная команда парсится нормально.
func TestParse_EmptyInline_Consumed(t *testing.T) {
	cb := newTestParser([]byte("\r\nPING\r\n"))

	first := cb.ParseCommand()
	if first == nil {
		t.Fatal("M3: пустая inline вернула nil → rpos откатится → wedge")
	}
	if len(first) != 0 {
		t.Fatalf("ожидали пустой набор аргументов на пустой строке, got %v", first)
	}
	if cb.ProtoErr() {
		t.Fatal("пустая inline не должна рвать соединение (это не битый кадр)")
	}

	// Следующая команда должна распарситься — значит пустая строка потреблена.
	second := cb.ParseCommand()
	if len(second) != 1 || string(second[0]) != "PING" {
		t.Fatalf("M3: следующая команда после пустой строки не распарсилась: %v (пустая строка не потреблена → wedge)", second)
	}
}
