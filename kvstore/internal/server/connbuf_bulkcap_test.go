package server

import "testing"

// TestParse_OversizedBulkLength_ProtoErr — репро mem-DoS (remote unauth OOM).
//
// parseBulkString проверяет только size < 0. Любое положительное size принимается
// без верхней границы. На "$2000000000\r\n" (≈2 ГБ bulk) данных в буфере ещё нет:
//
//	end := cb.rpos + size + 2
//	if end > cb.rend { return nil }   // ← "ждём ещё данные"
//
// nil трактуется handleConn как "неполный кадр" → греется read-слой, который при
// заполнении буфера УДВАИВАЕТ его (connbuf.go: ReadFromConn / TryRead):
//
//	newBuf := make([]byte, len(cb.rbuf)*2)
//
// Один неаутентифицированный клиент, объявив гигантский bulk и капая байтами,
// гонит rbuf к OOM — парсинг идёт ДО AUTH. Кэпа на bulk-длину нет (Redis тут
// держит proto-max-bulk-len = 512 МБ).
//
// Корректное поведение: пометить protoErr (как на size < -1), чтобы handleConn
// ответил ошибкой и закрыл соединение, а не пытался «дождать» 2 ГБ.
func TestParse_OversizedBulkLength_ProtoErr(t *testing.T) {
	input := []byte("*1\r\n$2000000000\r\n") // массив из 1 bulk-string длиной ~2 ГБ
	cb := newTestParser(input)

	got := cb.ParseCommand()
	if got != nil {
		t.Fatalf("ожидали nil на превышающей кэп bulk-длине, получили %v", got)
	}
	if !cb.ProtoErr() {
		t.Fatal("ProtoErr не выставлен на огромной bulk-длине ($2000000000) — " +
			"read-слой будет бесконечно удваивать rbuf → remote unauth OOM (mem-DoS)")
	}
	// Регрессию легитимных размеров сторожат TestParse_SimpleSet / большие векторы.
}

// TestParse_OversizedArrayCount_ProtoErr — родственный mem-DoS/wedge через count.
//
// parseArray на count > maxArgs возвращает nil без protoErr. Кадр выглядит как
// «недочитанный» и навсегда застревает в буфере, держа соединение и память,
// вместо честного разрыва. Должно вести себя как огромная bulk-длина: protoErr.
func TestParse_OversizedArrayCount_ProtoErr(t *testing.T) {
	input := []byte("*2000000000\r\n") // объявлен массив из ~2 млрд аргументов
	cb := newTestParser(input)

	got := cb.ParseCommand()
	if got != nil {
		t.Fatalf("ожидали nil на превышающем maxArgs count, получили %v", got)
	}
	if !cb.ProtoErr() {
		t.Fatal("ProtoErr не выставлен на count > maxArgs (*2000000000) — " +
			"кадр зависнет в буфере вместо закрытия соединения")
	}
}
