package vector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unsafe"
)

// VSearchResult — один результат поиска.
type VSearchResult struct {
	Key      string
	Distance float32
}

// SerializeVector кодирует вектор для WAL-операции OpVSimAdd: float32 LE подряд.
func SerializeVector(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func DeserializeVector(data []byte) []float32 {
	n := len(data) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(data[i*4:])
		vec[i] = math.Float32frombits(bits)
	}
	return vec
}

func DeserializeVectorZeroCopy(data []byte) []float32 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), len(data)/4)
}

// SerializeVectorWithAttrs кодирует вектор + атрибуты в один self-contained блоб
// для WAL-операции OpVSimAddAttrs. Формат: [uint32 nFloats][vec float32 LE...][attrs].
// Секция attrs использует тот же формат, что и снапшот (writeAttrs) → атрибуты
// становятся durable через WAL так же, как через снапшот (закрывает P0-4: replay
// больше не теряет attrs/tenant у векторов, добавленных после последнего снапшота).
func SerializeVectorWithAttrs(vec []float32, attrs Attributes) []byte {
	var buf bytes.Buffer
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(vec)))
	buf.Write(u4[:])
	for _, v := range vec {
		binary.LittleEndian.PutUint32(u4[:], math.Float32bits(v))
		buf.Write(u4[:])
	}
	// writeAttrs пишет в io.Writer; на bytes.Buffer ошибки не бывает.
	_ = writeAttrs(&buf, attrs)
	return buf.Bytes()
}

// SerializeVectorWithDoc кодирует вектор + атрибуты + термы текста в
// self-contained блоб для WAL-операции OpVSimAddDoc. Формат = формат
// OpVSimAddAttrs + секция terms (writeTerms) в хвосте: [uint32 nFloats]
// [vec float32 LE...][attrs][terms]. Термы, не сырой текст — см. wal.go.
func SerializeVectorWithDoc(vec []float32, attrs Attributes, terms []TermTF) []byte {
	return appendVectorWithDoc(nil, vec, attrs, terms)
}

// sliceWriter — io.Writer поверх растущего среза.
//
// Существует ровно затем, чтобы КОДИРОВЩИК ОСТАЛСЯ ОДИН. Соблазн написать
// append-версию writeAttrs/writeTerms руками велик, но два независимых
// кодировщика одного формата расходятся молча: правку внесут в один, читатель
// продолжит понимать оба до первого нового поля. Здесь append-путь и
// io.Writer-путь — физически один и тот же код, разное только назначение.
//
// WriteString не для красоты: writeStr зовёт io.WriteString, который проверяет
// io.StringWriter; без метода каждая строка конвертировалась бы в []byte
// отдельной аллокацией.
type sliceWriter struct{ b []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func (w *sliceWriter) WriteString(s string) (int, error) {
	w.b = append(w.b, s...)
	return len(s), nil
}

// appendVectorWithDoc — та же кодировка, что SerializeVectorWithDoc, но в
// переданный буфер (dst может быть nil).
//
// Зачем: путь снапшота сериализует документы пачкой по 10k подряд, и свежий
// bytes.Buffer на каждый стоил 36 МБ мусора на корпус, чей снапшот весит 17 МБ
// (профиль 01.08) — вектор писался по 4 байта в буфер, растущий удвоением с
// нуля, а потом результат ещё раз копировался во второй буфер под термы.
// ⚠Сверять с SerializeVectorWithDoc бессмысленно — та теперь сама зовёт эту.
// Эталон в serialize_append_test.go собран из ПРЕЖНЕЙ композиции
// (SerializeVectorWithAttrs + writeTerms, обе нетронуты): расхождение
// кодировщиков — это порча снапшота, которую содержательные проверки не видят,
// и ловить её обязана байтовая сверка со старым способом, а не с самой собой.
func appendVectorWithDoc(dst []byte, vec []float32, attrs Attributes, terms []TermTF) []byte {
	w := &sliceWriter{b: dst}
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(vec)))
	_, _ = w.Write(u4[:])
	for _, v := range vec {
		binary.LittleEndian.PutUint32(u4[:], math.Float32bits(v))
		_, _ = w.Write(u4[:])
	}
	// writeAttrs/writeTerms пишут в io.Writer; на sliceWriter ошибки не бывает.
	_ = writeAttrs(w, attrs)
	_ = writeTerms(w, terms)
	return w.b
}

// DeserializeVectorWithDoc — обратная операция для replay OpVSimAddDoc.
func DeserializeVectorWithDoc(data []byte) ([]float32, Attributes, []TermTF, error) {
	r := bytes.NewReader(data)
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, Attributes{}, nil, fmt.Errorf("vec+doc: read nFloats: %w", err)
	}
	n := int(binary.LittleEndian.Uint32(u4[:]))
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, Attributes{}, nil, fmt.Errorf("vec+doc: read float[%d]: %w", i, err)
		}
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(u4[:]))
	}
	attrs, err := readAttrs(r)
	if err != nil {
		return nil, Attributes{}, nil, fmt.Errorf("vec+doc: read attrs: %w", err)
	}
	terms, err := readTerms(r)
	if err != nil {
		return nil, Attributes{}, nil, fmt.Errorf("vec+doc: read terms: %w", err)
	}
	return vec, attrs, terms, nil
}

// DeserializeVectorWithAttrs — обратная операция для replay OpVSimAddAttrs.
func DeserializeVectorWithAttrs(data []byte) ([]float32, Attributes, error) {
	r := bytes.NewReader(data)
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, Attributes{}, fmt.Errorf("vec+attrs: read nFloats: %w", err)
	}
	n := int(binary.LittleEndian.Uint32(u4[:]))
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, Attributes{}, fmt.Errorf("vec+attrs: read float[%d]: %w", i, err)
		}
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(u4[:]))
	}
	attrs, err := readAttrs(r)
	if err != nil {
		return nil, Attributes{}, fmt.Errorf("vec+attrs: read attrs: %w", err)
	}
	return vec, attrs, nil
}
