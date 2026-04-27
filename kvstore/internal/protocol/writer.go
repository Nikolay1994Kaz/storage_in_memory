package protocol

import (
	"bufio"
	"io"
)

// Writer — буферизированный RESP writer.
//
// Зачем bufio.Writer?
//
// Без буферизации каждый Write() = один системный вызов write().
// При pipeline из 16 команд — 16 syscall'ов.
//
// С bufio.Writer:
//
//	Write() → копирует в буфер (userspace, ~5ns)
//	Flush() → ОДИН syscall write() для всех ответов разом
//
// Для pipeline это критично:
//
//	16 ответов × 1 syscall = 16 syscall'ов без bufio
//	16 ответов × 0 syscall + 1 Flush = 1 syscall с bufio
//
// Каждый syscall ≈ 1-2μs (переключение user→kernel→user).
// 16 syscall'ов = 16-32μs. 1 syscall = 1-2μs. Разница 10-16x.
type Writer struct {
	writer *bufio.Writer
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{writer: bufio.NewWriter(w)}
}

// Write буферизирует один RESP-ответ.
//
// Данные НЕ отправляются клиенту сразу.
// Они накапливаются в буфере до вызова Flush().
//
// Без pipeline: Write() → Flush() (сразу после одной команды)
// С pipeline:   Write() × N → Flush() (все ответы за один раз)
func (w *Writer) Write(v Value) error {
	bytes := v.Marshal()
	_, err := w.writer.Write(bytes)
	return err
}

// Flush отправляет все накопленные ответы клиенту за один write().
//
// Вызывается:
//   - После каждой команды (без pipeline)
//   - После ВСЕХ команд в pipeline (с pipeline)
//
// Под капотом bufio.Writer.Flush():
//  1. Собирает все данные из внутреннего буфера
//  2. Вызывает conn.Write(buf) — ОДИН системный вызов
//  3. Очищает буфер
func (w *Writer) Flush() error {
	return w.writer.Flush()
}
