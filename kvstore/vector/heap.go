package vector

import "container/heap"

// item — пара: ID ноды + расстояние до точки запроса.
//
// Это «конверт» с двумя полями. Когда мы ищем ближайших соседей,
// нам нужно знать: КОГО мы нашли (id) и НАСКОЛЬКО далеко он (dist).
type item struct {
	id   uint64
	dist float32
}

// ─── minHeap: ближайший элемент = первый ───
//
// Используется для КАНДИДАТОВ при поиске.
// «Дай мне следующего самого близкого кандидата для проверки».
//
// Ты уже видел этот паттерн в TTL MinHeap (store/ttl_heap.go):
// там heap отдаёт ключ с НАИМЕНЬШИМ временем жизни.
// Здесь — элемент с НАИМЕНЬШИМ расстоянием.
type minHeap []item

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].dist < h[j].dist } // меньше = ближе = приоритетнее
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// ─── maxHeap: самый далёкий элемент = первый ───
//
// Используется для РЕЗУЛЬТАТОВ поиска.
// «У меня есть ef лучших результатов. Если новый кандидат ближе,
// чем мой ХУДШИЙ результат — выкинь худший, добавь нового».
//
// Единственная разница с minHeap — знак в Less():
//
//	minHeap: h[i].dist < h[j].dist  (ближайший первый)
//	maxHeap: h[i].dist > h[j].dist  (дальний первый)
type maxHeap []item

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(i, j int) bool { return h[i].dist > h[j].dist } // больше = дальше = приоритетнее
func (h maxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *maxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// Проверка на этапе компиляции: наши типы реализуют heap.Interface.
// Если забудем метод — компилятор скажет ошибку.
var _ heap.Interface = (*minHeap)(nil)
var _ heap.Interface = (*maxHeap)(nil)
