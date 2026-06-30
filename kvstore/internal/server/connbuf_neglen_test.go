package server

import "testing"

// TestParse_NegativeBulkLength_NoPanic — репро P0-2 (remote unauth crash).
//
// parseIntBytes принимает отрицательные числа (для $-1 = null bulk). Но в
// parseBulkString спец-кейсом обрабатывается только size == -1; любое другое
// отрицательное (например -2) проходит дальше:
//
//	end := cb.rpos + size + 2     // = rpos + (-2) + 2 = rpos
//	if end > cb.rend { return nil } // НЕ срабатывает (end == rpos <= rend)
//	data := cb.rbuf[cb.rpos : cb.rpos+size] // rbuf[rpos : rpos-2] → panic
//
// Парсинг идёт в ParseCommand() ДО AUTH-проверки, а recover() в worker-цикле
// (server.go: eventLoop → handleConn) нет → один пакет от неаутентифицированного
// клиента кладёт весь процесс.
//
// Корректное поведение: вернуть nil (как на битые/неполные данные), не паниковать.
func TestParse_NegativeBulkLength_NoPanic(t *testing.T) {
	input := []byte("*1\r\n$-2\r\n") // массив из 1 bulk-string длиной -2
	cb := newTestParser(input)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseCommand паникует на отрицательной bulk-длине ($-2): %v\n"+
				"→ remote unauthenticated DoS: один пакет роняет процесс (нет recover в worker-цикле)", r)
		}
	}()

	if got := cb.ParseCommand(); got != nil {
		t.Fatalf("ожидали nil на битом кадре, получили %v", got)
	}
	// Битый кадр нельзя «дождать» — парсер обязан пометить протокол-ошибку,
	// чтобы handleConn закрыл соединение (иначе тихий зависон + рост буфера).
	if !cb.ProtoErr() {
		t.Fatal("ProtoErr не выставлен на невалидной bulk-длине — соединение зависнет вместо закрытия")
	}
	// Регрессию легитимного $-1 (null bulk) сторожит TestParse_NullBulkString.
}
