package auditchain

import (
	"testing"
)

// buildChain — цепь из n событий поверх пустой головы.
func buildChain(n int) ([]Record, Head) {
	var head Head
	recs := make([]Record, 0, n)
	for i := 0; i < n; i++ {
		r := Link(head, int64(1700000000+i), EventRemember, "alice", "fact-"+string(rune('a'+i)), []byte("payload"))
		recs = append(recs, r)
		head = Head{Seq: r.Seq, Hash: Hash(r)}
	}
	return recs, head
}

func TestVerify_AcceptsIntactChain(t *testing.T) {
	recs, head := buildChain(5)
	got, err := Verify(recs, &head)
	if err != nil {
		t.Fatalf("целая цепь отвергнута: %v", err)
	}
	if got.Seq != 5 {
		t.Errorf("голова seq=%d, ожидалось 5", got.Seq)
	}
}

// TestVerify_CatchesRewrittenPast — то, ради чего цепь существует.
func TestVerify_CatchesRewrittenPast(t *testing.T) {
	recs, head := buildChain(5)
	recs[2].Subject = "подменённый факт" // правка середины

	if _, err := Verify(recs, &head); err == nil {
		t.Fatal("подмена записи в середине цепи не обнаружена")
	}
}

// TestVerify_CatchesTruncatedTail — дыра 2 из шапки пакета: отрезанный хвост
// оставляет цепь математически валидной, и только сохранённая голова его
// выдаёт.
func TestVerify_CatchesTruncatedTail(t *testing.T) {
	recs, head := buildChain(5)
	truncated := recs[:3]

	// ПАРНЫЙ КОНТРОЛЬ: без головы обрезанная цепь безупречна — именно поэтому
	// одной цепи мало, и именно это делает голову обязательной, а не
	// украшением.
	if _, err := Verify(truncated, nil); err != nil {
		t.Fatalf("обрезанная цепь сама по себе обязана быть валидной, иначе тест ниже проверяет не то: %v", err)
	}
	if _, err := Verify(truncated, &head); err == nil {
		t.Fatal("обрезка хвоста не обнаружена при сверке с сохранённой головой")
	}
}

// TestVerify_CatchesGapInSeq — вырезанная из середины запись.
func TestVerify_CatchesGapInSeq(t *testing.T) {
	recs, head := buildChain(5)
	gapped := append(append([]Record{}, recs[:2]...), recs[3:]...)

	if _, err := Verify(gapped, &head); err == nil {
		t.Fatal("вырезанная из середины запись не обнаружена")
	}
}

// ⭐TestHash_FieldBoundariesAreUnambiguous — дыра 1: без префиксов длины две
// разные записи дают ОДИН хеш, и подмена проходит проверку.
//
// ⚠Пара подобрана так, что записи совпадают при ЛЮБОЙ склейке без длин:
// "ab"+"c" и "a"+"bc" дают "abc" и при простой конкатенации, и через любой
// разделитель, которого нет в данных. Первая версия теста брала "a|b"+"c"
// против "a"+"b|c" — она ловила только склейку через «|», а мутация
// «конкатенация без разделителя вовсе» прошла мимо. Тест на подмену границ
// обязан не зависеть от того, КАКОЙ разделитель выбрал бы автор дефекта.
func TestHash_FieldBoundariesAreUnambiguous(t *testing.T) {
	var head Head
	left := Link(head, 1700000000, EventRemember, "ab", "c", nil)
	right := Link(head, 1700000000, EventRemember, "a", "bc", nil)

	if Hash(left) == Hash(right) {
		t.Fatal("две разные записи дали одинаковый хеш — границы полей неоднозначны")
	}

	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: одинаковые записи обязаны давать
	// одинаковый хеш, иначе «хеши различны» верно и для сломанного хеша,
	// возвращающего случайное.
	same := Link(head, 1700000000, EventRemember, "ab", "c", nil)
	if Hash(left) != Hash(same) {
		t.Fatal("одинаковые записи дали разные хеши — хеш недетерминирован")
	}
}

// TestHash_CoversEveryField — ни одно поле не должно выпадать из хеша: поле
// вне хеша можно менять безнаказанно, и цепь этого не заметит.
func TestHash_CoversEveryField(t *testing.T) {
	var head Head
	base := Link(head, 1700000000, EventRemember, "alice", "fact-1", []byte("payload"))

	variants := map[string]Record{
		"seq":      {Seq: base.Seq + 1, PrevHash: base.PrevHash, UnixNano: base.UnixNano, Type: base.Type, Scope: base.Scope, Subject: base.Subject, Payload: base.Payload},
		"prevHash": {Seq: base.Seq, PrevHash: [32]byte{9}, UnixNano: base.UnixNano, Type: base.Type, Scope: base.Scope, Subject: base.Subject, Payload: base.Payload},
		"time":     {Seq: base.Seq, PrevHash: base.PrevHash, UnixNano: base.UnixNano + 1, Type: base.Type, Scope: base.Scope, Subject: base.Subject, Payload: base.Payload},
		"type":     {Seq: base.Seq, PrevHash: base.PrevHash, UnixNano: base.UnixNano, Type: EventShred, Scope: base.Scope, Subject: base.Subject, Payload: base.Payload},
		"scope":    {Seq: base.Seq, PrevHash: base.PrevHash, UnixNano: base.UnixNano, Type: base.Type, Scope: "bob", Subject: base.Subject, Payload: base.Payload},
		"subject":  {Seq: base.Seq, PrevHash: base.PrevHash, UnixNano: base.UnixNano, Type: base.Type, Scope: base.Scope, Subject: "fact-2", Payload: base.Payload},
		"payload":  {Seq: base.Seq, PrevHash: base.PrevHash, UnixNano: base.UnixNano, Type: base.Type, Scope: base.Scope, Subject: base.Subject, Payload: []byte("другое")},
	}
	for name, v := range variants {
		if Hash(v) == Hash(base) {
			t.Errorf("поле %s не входит в хеш — его можно менять незаметно", name)
		}
	}
}

// TestVerify_CatchesSeqGapWithValidHashes — проверка нумерации нужна отдельно
// от проверки связи, и вот случай, который ловится ТОЛЬКО ею: запись собрана
// вручную с пропущенным seq, но с корректным PrevHash, поэтому хеши сходятся.
// Без этого теста мутация «не проверять seq» проходит мимо: обычные подделки
// ломают ещё и связь, и их ловит соседняя проверка.
func TestVerify_CatchesSeqGapWithValidHashes(t *testing.T) {
	recs, _ := buildChain(3)
	last := recs[2]

	jumped := Record{
		Seq:      last.Seq + 2, // пропуск: 3 → 5
		PrevHash: Hash(last),   // связь при этом ЧЕСТНАЯ
		UnixNano: 1700000500,
		Type:     EventShred,
		Scope:    "alice",
	}
	if _, err := Verify(append(recs, jumped), nil); err == nil {
		t.Fatal("пропуск в нумерации при сходящихся хешах не обнаружен")
	}
}

// TestLink_ChainsOntoHead — связь строится только через Link, поэтому «забыть
// связать» нельзя: seq и prevHash берутся из головы.
func TestLink_ChainsOntoHead(t *testing.T) {
	recs, head := buildChain(3)
	next := Link(head, 1700000100, EventShred, "alice", "", []byte("receipt"))

	if next.Seq != 4 {
		t.Errorf("seq=%d, ожидался 4", next.Seq)
	}
	if next.PrevHash != Hash(recs[2]) {
		t.Error("новая запись не ссылается на хеш последней")
	}
	if _, err := Verify(append(recs, next), nil); err != nil {
		t.Errorf("цепь с дописанной записью не сходится: %v", err)
	}
}
