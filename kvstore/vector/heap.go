package vector

// item — пара: ID ноды + расстояние до точки запроса.
type item struct {
	id   uint64
	dist float32
}

// ═════════════════════════════════════════════════════════
// minHeap — ближайший элемент = первый (корень).
// ═════════════════════════════════════════════════════════
//
// TYPED реализация: push/pop работают напрямую с item.
// Без container/heap, без any, без interface boxing = ZERO ALLOC.

type minHeap []item

func (h *minHeap) Len() int { return len(*h) }

// peek возвращает минимальный элемент БЕЗ удаления.
func (h *minHeap) peek() item { return (*h)[0] }

// push добавляет элемент и восстанавливает свойство кучи.
//
// 1. Добавляем в конец массива
// 2. «Всплываем» наверх: если новый < родителя — меняем местами
func (h *minHeap) push(it item) {
	*h = append(*h, it)
	h.siftUp(len(*h) - 1)
}

// pop удаляет и возвращает минимальный элемент.
//
// 1. Запоминаем корень (минимум)
// 2. Ставим последний элемент на место корня
// 3. «Топим» вниз: если новый корень > ребёнка — меняем с меньшим
func (h *minHeap) pop() item {
	old := *h
	n := len(old)
	result := old[0]  // запоминаем корень
	old[0] = old[n-1] // последний → на место корня
	*h = old[:n-1]    // уменьшаем размер
	if len(*h) > 0 {
		h.siftDown(0) // восстанавливаем свойство кучи
	}
	return result
}

// siftUp — «всплытие». Элемент i поднимается к корню,
// пока он меньше своего родителя.
func (h *minHeap) siftUp(i int) {
	data := *h
	for i > 0 {
		parent := (i - 1) / 2
		if data[parent].dist <= data[i].dist {
			break // родитель меньше или равен — порядок верный
		}
		data[parent], data[i] = data[i], data[parent]
		i = parent
	}
}

// siftDown — «погружение». Элемент i опускается вниз,
// пока он больше одного из своих детей.
func (h *minHeap) siftDown(i int) {
	data := *h
	n := len(data)
	for {
		smallest := i
		left := 2*i + 1
		right := 2*i + 2

		if left < n && data[left].dist < data[smallest].dist {
			smallest = left
		}
		if right < n && data[right].dist < data[smallest].dist {
			smallest = right
		}
		if smallest == i {
			break // элемент на своём месте
		}
		data[i], data[smallest] = data[smallest], data[i]
		i = smallest
	}
}

// ═════════════════════════════════════════════════════════
// maxHeap — самый далёкий элемент = первый (корень).
// ═════════════════════════════════════════════════════════
//
// Единственная разница с minHeap: знаки сравнения ОБРАТНЫЕ.

type maxHeap []item

func (h *maxHeap) Len() int { return len(*h) }

func (h *maxHeap) peek() item { return (*h)[0] }

func (h *maxHeap) push(it item) {
	*h = append(*h, it)
	h.siftUp(len(*h) - 1)
}

func (h *maxHeap) pop() item {
	old := *h
	n := len(old)
	result := old[0]
	old[0] = old[n-1]
	*h = old[:n-1]
	if len(*h) > 0 {
		h.siftDown(0)
	}
	return result
}

func (h *maxHeap) siftUp(i int) {
	data := *h
	for i > 0 {
		parent := (i - 1) / 2
		if data[parent].dist >= data[i].dist { // ← ОБРАТНЫЙ знак
			break
		}
		data[parent], data[i] = data[i], data[parent]
		i = parent
	}
}

func (h *maxHeap) siftDown(i int) {
	data := *h
	n := len(data)
	for {
		largest := i
		left := 2*i + 1
		right := 2*i + 2

		if left < n && data[left].dist > data[largest].dist { // ← ОБРАТНЫЙ знак
			largest = left
		}
		if right < n && data[right].dist > data[largest].dist {
			largest = right
		}
		if largest == i {
			break
		}
		data[i], data[largest] = data[largest], data[i]
		i = largest
	}
}
