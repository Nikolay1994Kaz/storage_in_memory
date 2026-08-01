// =============================================================================
// appendVectorWithDoc ≡ прежняя кодировка, БАЙТ В БАЙТ.
//
// Правка 01.08 убрала промежуточный bytes.Buffer на каждый документ (36 МБ
// мусора на корпус). Она не должна была изменить ни одного байта — но «не
// должна была» проверяется, а не заявляется: снапшот с разъехавшимся форматом
// читается без ошибки ровно до того места, где смещения перестают сходиться.
//
// ⚠ЧТО ЭТОТ ОРАКУЛ ЛОВИТ И ЧЕГО НЕ ЛОВИТ. Эталон собран из НЕТРОНУТЫХ
// SerializeVectorWithAttrs (bytes.Buffer) + writeTerms — то есть проверяется
// именно перекладка труб. Общий код у сторон есть (writeAttrs/writeTerms), и
// мутация ВНУТРИ них сдвинет обе стороны одинаково — этот оракул слеп к ней
// по построению. Ту ось держат золотые снапшоты v8/v9 в testdata: там байты
// сняты старой сборкой и общего кода со свежей не имеют.
// =============================================================================
package vector

import (
	"bytes"
	"testing"
)

// serializeVectorWithDocOld — кодировка ДО правки, дословно.
func serializeVectorWithDocOld(vec []float32, attrs Attributes, terms []TermTF) []byte {
	buf := bytes.NewBuffer(SerializeVectorWithAttrs(vec, attrs))
	_ = writeTerms(buf, terms)
	return buf.Bytes()
}

func appendDocCases() []struct {
	name  string
	vec   []float32
	attrs Attributes
	terms []TermTF
} {
	return []struct {
		name  string
		vec   []float32
		attrs Attributes
		terms []TermTF
	}{
		{"пусто везде", nil, Attributes{}, nil},
		{"только вектор", []float32{1.5, -2.25, 0}, Attributes{}, nil},
		{"вектор+кат", []float32{0.1}, Attributes{Cat: map[string]string{"scope": "alice"}}, nil},
		{"вектор+числа", []float32{0.1}, Attributes{Num: map[string]float64{"valid_from": 1.75e9}}, nil},
		{"только термы", nil, Attributes{}, []TermTF{{Term: "aurora", TF: 3}}},
		{
			"всё сразу, несколько имён",
			[]float32{1, 2, 3, 4, 5},
			Attributes{
				// Больше одного имени: порядок обхода map недетерминирован,
				// и сортировка внутри writeAttrs — часть проверяемого формата.
				Cat: map[string]string{"scope": "bob", "source": "chan-zephyr", "type": "fact"},
				Num: map[string]float64{"importance": 0.5, "valid_from": 1.75e9},
			},
			[]TermTF{{Term: "standup", TF: 1}, {Term: "zephyr", TF: 2}},
		},
		{"пустая строка значения", []float32{7}, Attributes{Cat: map[string]string{"lang": ""}}, nil},
	}
}

func TestAppendVectorWithDocBytesIdentical(t *testing.T) {
	for _, tc := range appendDocCases() {
		t.Run(tc.name, func(t *testing.T) {
			want := serializeVectorWithDocOld(tc.vec, tc.attrs, tc.terms)
			got := appendVectorWithDoc(nil, tc.vec, tc.attrs, tc.terms)
			if !bytes.Equal(got, want) {
				t.Fatalf("байты разошлись:\n old(%d) = %x\n new(%d) = %x", len(want), want, len(got), got)
			}
			// Контроль диагноза: пустой ожидаемый блоб сделал бы сверку
			// бессодержательной (nil == nil прошло бы при любой ошибке).
			if len(want) == 0 {
				t.Fatal("эталон пуст — сверка ничего не доказывает")
			}
			// Публичная обёртка обязана давать то же самое.
			if pub := SerializeVectorWithDoc(tc.vec, tc.attrs, tc.terms); !bytes.Equal(pub, want) {
				t.Fatalf("SerializeVectorWithDoc разошёлся с эталоном: %x vs %x", pub, want)
			}
		})
	}
}

// TestAppendVectorWithDocPreservesPrefix — свойство, ради которого правка и
// делалась: документы кодируются В ОДИН буфер подряд. Если append затрёт
// префикс или потеряет хвост, снапшот развалится на границе документов.
func TestAppendVectorWithDocPreservesPrefix(t *testing.T) {
	prefix := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	cases := appendDocCases()

	buf := append([]byte(nil), prefix...)
	var want []byte
	want = append(want, prefix...)
	for _, tc := range cases {
		buf = appendVectorWithDoc(buf, tc.vec, tc.attrs, tc.terms)
		want = append(want, serializeVectorWithDocOld(tc.vec, tc.attrs, tc.terms)...)
	}
	if !bytes.Equal(buf, want) {
		t.Fatalf("конкатенация разошлась:\n want(%d)\n got (%d)", len(want), len(buf))
	}
	if !bytes.HasPrefix(buf, prefix) {
		t.Fatal("префикс затёрт")
	}

	// Переиспользование буфера (buf[:0]) не должно тащить хвост прошлой группы.
	reused := buf[:0]
	reused = appendVectorWithDoc(reused, cases[1].vec, cases[1].attrs, cases[1].terms)
	if w := serializeVectorWithDocOld(cases[1].vec, cases[1].attrs, cases[1].terms); !bytes.Equal(reused, w) {
		t.Fatalf("после buf[:0] остался хвост: got %x, want %x", reused, w)
	}
}

// TestAppendVectorWithDocRoundTrip — читатель понимает записанное.
func TestAppendVectorWithDocRoundTrip(t *testing.T) {
	for _, tc := range appendDocCases() {
		t.Run(tc.name, func(t *testing.T) {
			blob := appendVectorWithDoc(nil, tc.vec, tc.attrs, tc.terms)
			vec, attrs, terms, err := DeserializeVectorWithDoc(blob)
			if err != nil {
				t.Fatalf("DeserializeVectorWithDoc: %v", err)
			}
			if len(vec) != len(tc.vec) {
				t.Fatalf("вектор: len=%d, ждём %d", len(vec), len(tc.vec))
			}
			for i := range vec {
				if vec[i] != tc.vec[i] {
					t.Fatalf("вектор[%d]=%v, ждём %v", i, vec[i], tc.vec[i])
				}
			}
			if len(attrs.Cat) != len(tc.attrs.Cat) || len(attrs.Num) != len(tc.attrs.Num) {
				t.Fatalf("атрибуты: cat=%d/num=%d, ждём %d/%d",
					len(attrs.Cat), len(attrs.Num), len(tc.attrs.Cat), len(tc.attrs.Num))
			}
			for k, v := range tc.attrs.Cat {
				if attrs.Cat[k] != v {
					t.Fatalf("cat[%q]=%q, ждём %q", k, attrs.Cat[k], v)
				}
			}
			for k, v := range tc.attrs.Num {
				if attrs.Num[k] != v {
					t.Fatalf("num[%q]=%v, ждём %v", k, attrs.Num[k], v)
				}
			}
			if len(terms) != len(tc.terms) {
				t.Fatalf("термы: len=%d, ждём %d", len(terms), len(tc.terms))
			}
			for i := range terms {
				if terms[i] != tc.terms[i] {
					t.Fatalf("терм[%d]=%v, ждём %v", i, terms[i], tc.terms[i])
				}
			}
		})
	}
}
