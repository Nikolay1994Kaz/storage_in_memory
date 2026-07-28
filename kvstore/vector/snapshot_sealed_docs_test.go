package vector

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeCrypto — кейринг-заглушка. Настоящий AES не нужен: проверяется НЕ
// криптография (она измерена и покрыта в internal/keyring), а то, что
// шифруется правильное под правильным ключом и что уничтоженный ключ роняет
// ровно свою группу.
//
// ⚠Но payload заглушка обязана РЕАЛЬНО преобразовывать. Первая версия просто
// приписывала префикс, оставляя содержимое открытым, — и проверка «терма нет
// в байтах» упала, поймав саму заглушку. Урок тот же, что записан про
// нейтральные данные: подделка, которая ничего не меняет, делает зелёным
// тест, который ничего не проверяет. XOR здесь — не защита, а гарантия, что
// открытый текст не может попасть в снапшот мимо Seal.
type fakeCrypto struct {
	destroyed map[string]bool
	sealCalls []string // под какими скоупами запечатывали — для проверок
}

func newFakeCrypto() *fakeCrypto {
	return &fakeCrypto{destroyed: map[string]bool{}}
}

// xorMask — обратимое преобразование, скрывающее открытый текст. Именно
// обратимое: тест round-trip обязан оставаться настоящим.
func xorMask(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ 0x5A
	}
	return out
}

func (f *fakeCrypto) crypto() *SnapshotCrypto {
	return &SnapshotCrypto{
		Seal: func(scope string, plain []byte) ([]byte, error) {
			f.sealCalls = append(f.sealCalls, scope)
			return append([]byte("ENV:"+scope+":"), xorMask(plain)...), nil
		},
		Unseal: func(env []byte) ([]byte, bool, error) {
			if !bytes.HasPrefix(env, []byte("ENV:")) {
				return nil, false, errors.New("не конверт")
			}
			scopeAndBody, ok := bytes.CutPrefix(env, []byte("ENV:"))
			if !ok {
				return nil, false, errors.New("битый конверт")
			}
			scopeBytes, body, ok := bytes.Cut(scopeAndBody, []byte(":"))
			if !ok {
				return nil, false, errors.New("битый конверт")
			}
			if f.destroyed[string(scopeBytes)] {
				return nil, true, nil
			}
			return xorMask(body), false, nil
		},
	}
}

// sealedDocsFixture — документы трёх видов: два скоупа и бесскоупный вектор.
func sealedDocsFixture() ([]DeltaEntry, func(DeltaEntry) string) {
	entries := []DeltaEntry{
		{
			Key:   "fact-alice-1",
			Vec:   []float32{1, 2, 3, 4},
			Attrs: Attributes{Cat: map[string]string{vmemAttrScope: "alice"}, Num: map[string]float64{"imp": 0.7}},
			Terms: []TermTF{{Term: "aurora", TF: 2}, {Term: "contract", TF: 1}},
		},
		{
			Key:   "plain-vector",
			Vec:   []float32{9, 9, 9, 9},
			Attrs: Attributes{Cat: map[string]string{"lang": "ru"}},
		},
		{
			Key:   "fact-bob-1",
			Vec:   []float32{5, 6, 7, 8},
			Attrs: Attributes{Cat: map[string]string{vmemAttrScope: "bob"}},
			Terms: []TermTF{{Term: "standup", TF: 3}},
		},
		{
			Key:   "fact-alice-2",
			Vec:   []float32{2, 2, 2, 2},
			Attrs: Attributes{Cat: map[string]string{vmemAttrScope: "alice"}},
			Terms: []TermTF{{Term: "steering", TF: 1}},
		},
	}
	scopeOf := func(e DeltaEntry) string { return e.Attrs.Cat[vmemAttrScope] }
	return entries, scopeOf
}

func TestSealedDocsRoundTrip(t *testing.T) {
	entries, scopeOf := sealedDocsFixture()
	fc := newFakeCrypto()

	var buf bytes.Buffer
	if err := writeSealedDocs(&buf, entries, scopeOf, fc.crypto()); err != nil {
		t.Fatalf("writeSealedDocs: %v", err)
	}

	// Запечатаны ровно скоупы, и ровно по одному разу на скоуп: бесскоупная
	// группа шифроваться не должна — её нечем открыть обратно.
	if len(fc.sealCalls) != 2 {
		t.Fatalf("запечатано %d групп (%v), ожидалось 2", len(fc.sealCalls), fc.sealCalls)
	}

	got, dead, err := readSealedDocs(&buf, len(entries), fc.crypto())
	if err != nil {
		t.Fatalf("readSealedDocs: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("мёртвых %d, ожидалось 0: %v", len(dead), dead)
	}

	for i, want := range entries {
		if got[i].Key != want.Key {
			t.Errorf("позиция %d: ключ %q, ожидался %q", i, got[i].Key, want.Key)
		}
		if len(got[i].Vec) != len(want.Vec) {
			t.Fatalf("позиция %d: вектор длины %d, ожидалось %d", i, len(got[i].Vec), len(want.Vec))
		}
		for j := range want.Vec {
			if got[i].Vec[j] != want.Vec[j] {
				t.Errorf("позиция %d: вектор[%d] = %v, ожидалось %v", i, j, got[i].Vec[j], want.Vec[j])
			}
		}
		if got[i].Attrs.Cat[vmemAttrScope] != want.Attrs.Cat[vmemAttrScope] {
			t.Errorf("позиция %d: scope %q, ожидался %q", i,
				got[i].Attrs.Cat[vmemAttrScope], want.Attrs.Cat[vmemAttrScope])
		}
		if len(got[i].Terms) != len(want.Terms) {
			t.Errorf("позиция %d: термов %d, ожидалось %d", i, len(got[i].Terms), len(want.Terms))
		}
	}
}

// TestSealedDocsHideContent — отрицательная проверка С ПАРНЫМ положительным
// контролем: содержимое запечатанных групп в байтах отсутствует, а содержимое
// открытой группы — присутствует. Без второй половины тест был бы зелёным и
// при пустой записи.
func TestSealedDocsHideContent(t *testing.T) {
	entries, scopeOf := sealedDocsFixture()
	fc := newFakeCrypto()

	var buf bytes.Buffer
	if err := writeSealedDocs(&buf, entries, scopeOf, fc.crypto()); err != nil {
		t.Fatalf("writeSealedDocs: %v", err)
	}
	raw := buf.String()

	for _, secret := range []string{"aurora", "contract", "steering", "standup"} {
		if strings.Contains(raw, secret) {
			t.Errorf("терм %q найден в снапшоте открытым текстом", secret)
		}
	}
	// ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: мы вообще писали данные, и то, что шифровать
	// нечем, лежит открыто — значит проверка выше искала в правильном месте.
	if !strings.Contains(raw, "lang") {
		t.Error("атрибут бесскоупного документа не найден — секция пуста, проверка прошла бы по неверной причине")
	}
	// Ключи ОСТАЮТСЯ открытыми осознанно (без них мёртвых нечем пометить).
	if !strings.Contains(raw, "fact-alice-1") {
		t.Error("ключ факта не найден открытым — тогда мёртвые документы нечем положить в tombstones")
	}
}

func TestSealedDocsShredDropsOnlyItsScope(t *testing.T) {
	entries, scopeOf := sealedDocsFixture()
	fc := newFakeCrypto()

	var buf bytes.Buffer
	if err := writeSealedDocs(&buf, entries, scopeOf, fc.crypto()); err != nil {
		t.Fatalf("writeSealedDocs: %v", err)
	}

	fc.destroyed["alice"] = true // ключ скоупа уничтожен VMEM.SHRED

	got, dead, err := readSealedDocs(bytes.NewReader(buf.Bytes()), len(entries), fc.crypto())
	if err != nil {
		t.Fatalf("readSealedDocs: %v", err)
	}

	deadSet := map[string]bool{}
	for _, k := range dead {
		deadSet[k] = true
	}
	if !deadSet["fact-alice-1"] || !deadSet["fact-alice-2"] {
		t.Errorf("оба факта стёртого скоупа обязаны попасть в мёртвые, получено %v", dead)
	}
	if deadSet["fact-bob-1"] || deadSet["plain-vector"] {
		t.Errorf("стирание задело чужое: %v", dead)
	}

	for i, e := range entries {
		restored := got[i].Key != ""
		wantRestored := e.Attrs.Cat[vmemAttrScope] != "alice"
		if restored != wantRestored {
			t.Errorf("позиция %d (%s): восстановлен=%v, ожидалось %v", i, e.Key, restored, wantRestored)
		}
	}
	// Соседний скоуп восстановлен ЦЕЛИКОМ, а не только ключом.
	if len(got[2].Terms) != 1 || got[2].Terms[0].Term != "standup" {
		t.Errorf("факт соседнего скоупа восстановлен неполно: %+v", got[2].Terms)
	}
}

// TestSealedDocsRefusesWithoutKeyring — запечатанные данные без кейринга это
// НЕ «пропустить и продолжить»: снапшот прочитан бы неполно, а потеря данных,
// замаскированная под смену настройки, — худший из отказов.
func TestSealedDocsRefusesWithoutKeyring(t *testing.T) {
	entries, scopeOf := sealedDocsFixture()
	fc := newFakeCrypto()

	var buf bytes.Buffer
	if err := writeSealedDocs(&buf, entries, scopeOf, fc.crypto()); err != nil {
		t.Fatalf("writeSealedDocs: %v", err)
	}
	if _, _, err := readSealedDocs(bytes.NewReader(buf.Bytes()), len(entries), nil); err == nil {
		t.Fatal("запечатанный снапшот прочитан без кейринга и без ошибки")
	}
}

// TestSealedDocsWithoutCryptoStaysPlain — без -encrypt-at-rest поведение
// обязано остаться прежним байт в байт: ничего не шифруется, всё читается.
func TestSealedDocsWithoutCryptoStaysPlain(t *testing.T) {
	entries, scopeOf := sealedDocsFixture()

	var buf bytes.Buffer
	if err := writeSealedDocs(&buf, entries, scopeOf, nil); err != nil {
		t.Fatalf("writeSealedDocs: %v", err)
	}
	got, dead, err := readSealedDocs(bytes.NewReader(buf.Bytes()), len(entries), nil)
	if err != nil {
		t.Fatalf("readSealedDocs: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("без шифрования мёртвых быть не может: %v", dead)
	}
	for i, want := range entries {
		if got[i].Key != want.Key {
			t.Errorf("позиция %d: ключ %q, ожидался %q", i, got[i].Key, want.Key)
		}
	}
}

// TestSealedDocsKeepsHoles — дыры в сегменте (позиции без ключа: удалённые,
// tombstoned) не должны съезжать: слои строятся ПО ПОЗИЦИИ, и сдвиг на одну
// позицию перепутал бы атрибуты между документами.
func TestSealedDocsKeepsHoles(t *testing.T) {
	entries := []DeltaEntry{
		{Key: "a", Vec: []float32{1}, Attrs: Attributes{Cat: map[string]string{vmemAttrScope: "s"}}},
		{}, // дыра
		{Key: "c", Vec: []float32{3}, Attrs: Attributes{Cat: map[string]string{vmemAttrScope: "s"}}},
	}
	scopeOf := func(e DeltaEntry) string { return e.Attrs.Cat[vmemAttrScope] }
	fc := newFakeCrypto()

	var buf bytes.Buffer
	if err := writeSealedDocs(&buf, entries, scopeOf, fc.crypto()); err != nil {
		t.Fatalf("writeSealedDocs: %v", err)
	}
	got, _, err := readSealedDocs(bytes.NewReader(buf.Bytes()), len(entries), fc.crypto())
	if err != nil {
		t.Fatalf("readSealedDocs: %v", err)
	}
	if got[0].Key != "a" || got[1].Key != "" || got[2].Key != "c" {
		t.Errorf("позиции съехали: %q, %q, %q", got[0].Key, got[1].Key, got[2].Key)
	}
}

// TestSealedDocsDeterministic — два снапшота одного состояния обязаны быть
// байт в байт одинаковыми, иначе любая проверка целостности станет шумом.
func TestSealedDocsDeterministic(t *testing.T) {
	entries, scopeOf := sealedDocsFixture()
	fc := newFakeCrypto()

	var a, b bytes.Buffer
	for _, w := range []*bytes.Buffer{&a, &b} {
		if err := writeSealedDocs(w, entries, scopeOf, fc.crypto()); err != nil {
			t.Fatalf("writeSealedDocs: %v", err)
		}
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("две записи одного состояния разошлись побайтово")
	}

	// ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: сравнение выше что-то значит только если писалось
	// непустое — на двух пустых буферах оно верно всегда.
	if a.Len() == 0 {
		t.Fatal("секция пуста — детерминизм проверен на пустоте")
	}
}
