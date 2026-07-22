package vector

import "testing"

// Регресс шага 8: утечка закрытой supersession-версии через границу СЕГМЕНТОВ.
// Найдено корпус-бенчем (nowchain accuracy 0.023 до фикса): затенение
// сегмент-vs-сегмент в merge текстового поиска решается только среди хитов
// (bm25_store.go), а закрытую копию цели фильтр валидности отсекает ДО хитов —
// старая ОТКРЫТАЯ копия из старого сегмента воскресала как «истинная сейчас».
// Фикс: пере-суд кандидатов Recall по атрибутам свежайшей версии ключа
// (vmem.go). «Падает до фикса» проверено 22.07 на 3a444dd.
func TestVMEMSupersedeTwoSegmentLeak(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "u", Text: "alpha beta unique1"}, 100); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync() // сегмент A: открытый f1
	if _, err := lvs.Remember(RememberRequest{ID: "f2", Scope: "u", Text: "alpha beta unique2", Supersedes: "f1"}, 200); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync() // сегмент B: закрытый f1 + наследник f2

	// Дефолтный RECALL (валидное-сейчас): только наследник.
	res, err := lvs.Recall(RecallRequest{Scope: "u", Query: "alpha beta", K: 10}, 300)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Key == "f1" {
			t.Fatalf("закрытая версия f1 просочилась в дефолтный RECALL: %v", res)
		}
	}
	// AS_OF до замены обязан видеть ровно f1 (история жива, машина времени
	// работает сквозь supersession) — пере-суд не должен пересушить выдачу.
	asOf := int64(150)
	res, err = lvs.Recall(RecallRequest{Scope: "u", Query: "alpha beta", K: 10, AsOf: &asOf}, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Key != "f1" {
		t.Fatalf("AS_OF 150 обязан вернуть ровно f1, получено: %v", res)
	}
}
