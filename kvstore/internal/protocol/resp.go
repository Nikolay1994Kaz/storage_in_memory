package protocol

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type Value struct {
	Typ   byte
	Str   string
	Num   int
	Array []Value
}

type Reader struct {
	reader *bufio.Reader
}

func NewReader(rd io.Reader) *Reader {
	return &Reader{
		reader: bufio.NewReader(rd),
	}
}

func (r *Reader) Read() (Value, error) {
	typ, err := r.reader.ReadByte()
	if err != nil {
		return Value{}, err
	}
	switch typ {
	case '+':
		return r.readSimpleString()
	case '-':
		return r.readError()
	case ':':
		return r.readInteger()
	case '$':
		return r.readBulkString()
	case '*':
		return r.readArray()
	default:
		// Inline command: первый байт не является RESP-префиксом.
		// redis-benchmark и redis-cli могут отправлять команды в формате
		// "PING\r\n" или "SET key value\r\n" (inline protocol).
		// Читаем остаток строки, собираем с первым байтом и парсим как массив слов.
		r.reader.UnreadByte()
		return r.readInline()
	}
}

func (v Value) Marshal() []byte {
	switch v.Typ {
	case '+':
		return v.marshalSimpleString()
	case '-':
		return v.marshalError()
	case ':':
		return v.marshalInteger()
	case '$':
		return v.marshalBulkString()
	case '*':
		return v.marshalArray()
	default:
		return []byte{}
	}

}

func (v Value) marshalSimpleString() []byte {
	return []byte("+" + v.Str + "\r\n")
}

func (v Value) marshalError() []byte {
	return []byte("-" + v.Str + "\r\n")

}

func (v Value) marshalInteger() []byte {
	return []byte(":" + strconv.Itoa(v.Num) + "\r\n")
}

func (v Value) marshalBulkString() []byte {
	if v.Num == -1 {
		return []byte("$-1\r\n")
	}
	return []byte("$" + strconv.Itoa(len(v.Str)) + "\r\n" + v.Str + "\r\n")
}

func (v Value) marshalArray() []byte {
	// *3\r\n + каждый элемент
	buf := []byte("*" + strconv.Itoa(len(v.Array)) + "\r\n")
	for _, item := range v.Array {
		buf = append(buf, item.Marshal()...)
	}
	return buf
}

// readLine читает строку до \r\n и возвращает её БЕЗ \r\n.
// Это вспомогательный метод, который используют все парсеры.
func (r *Reader) readLine() (string, error) {
	line, err := r.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	// Убираем \r\n с конца
	if len(line) < 2 {
		return "", fmt.Errorf("invalid RESP line: too short")
	}
	return line[:len(line)-2], nil
}

// +OK\r\n → Value{Typ: '+', Str: "OK"}
func (r *Reader) readSimpleString() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	return Value{Typ: '+', Str: line}, nil
}

// -ERR unknown\r\n → Value{Typ: '-', Str: "ERR unknown"}
func (r *Reader) readError() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	return Value{Typ: '-', Str: line}, nil
}

// :42\r\n → Value{Typ: ':', Num: 42}
func (r *Reader) readInteger() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		return Value{}, fmt.Errorf("invalid integer: %s", line)
	}
	return Value{Typ: ':', Num: n}, nil
}

// $5\r\nhello\r\n → Value{Typ: '$', Str: "hello"}
// $-1\r\n         → Value{Typ: '$', Str: "", Num: -1}  (NULL)
func (r *Reader) readBulkString() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}

	size, err := strconv.Atoi(line)
	if err != nil {
		return Value{}, fmt.Errorf("invalid bulk string size: %s", line)
	}

	// $-1 означает NULL (ключ не найден)
	if size == -1 {
		return Value{Typ: '$', Str: "", Num: -1}, nil
	}

	// Читаем ровно size байт + 2 байта (\r\n)
	buf := make([]byte, size+2)
	_, err = io.ReadFull(r.reader, buf)
	if err != nil {
		return Value{}, err
	}

	return Value{Typ: '$', Str: string(buf[:size])}, nil
}

// *3\r\n... → Value{Typ: '*', Array: [...]}
func (r *Reader) readArray() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}

	count, err := strconv.Atoi(line)
	if err != nil {
		return Value{}, fmt.Errorf("invalid array count: %s", line)
	}

	// Рекурсивно читаем каждый элемент массива
	values := make([]Value, count)
	for i := 0; i < count; i++ {
		val, err := r.Read() // Рекурсия!
		if err != nil {
			return Value{}, err
		}
		values[i] = val
	}

	return Value{Typ: '*', Array: values}, nil
}

// readInline парсит inline-команду: "PING\r\n" или "SET key value\r\n".
// redis-benchmark отправляет команды в этом формате.
// Разбиваем строку по пробелам и оборачиваем в RESP-массив.
func (r *Reader) readInline() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}

	parts := strings.Fields(line)
	if len(parts) == 0 {
		return Value{}, fmt.Errorf("empty inline command")
	}

	values := make([]Value, len(parts))
	for i, part := range parts {
		values[i] = Value{Typ: '$', Str: part}
	}

	return Value{Typ: '*', Array: values}, nil
}
