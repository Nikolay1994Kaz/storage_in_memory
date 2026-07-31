package vector

// ============================================================================
// 1. VectorArena — Арена для хранения координат векторов (float32)
// ============================================================================

type VectorArena struct {
	data        []float32 // Единый плоский массив координат
	dim         int       // Размерность вектора (например, 128)
	freeOffsets []uint64  // Стек свободных смещений (Free List)
}

func NewVectorArena(dim int, initialCapacity int) *VectorArena {
	return &VectorArena{
		data:        make([]float32, 0, initialCapacity*dim),
		dim:         dim,
		freeOffsets: make([]uint64, 0, 64),
	}
}

// Allocate выделяет память под новый вектор.
//
// Паникует если len(vec) != dim — несоответствие размерности
// ломает весь layout арены (тихая порча данных).
func (va *VectorArena) Allocate(vec []float32) uint64 {
	if len(vec) != va.dim {
		panic("VectorArena.Allocate: dimension mismatch")
	}

	nFree := len(va.freeOffsets)
	if nFree > 0 {
		offset := va.freeOffsets[nFree-1]
		va.freeOffsets = va.freeOffsets[:nFree-1]
		copy(va.data[offset:offset+uint64(va.dim)], vec)
		return offset
	}

	offset := uint64(len(va.data))
	va.data = append(va.data, vec...)
	return offset
}

// Free освобождает ячейку вектора, ЗАТИРАЯ её нулями.
//
// Контракт: вызывающий код гарантирует, что offset не освобождается дважды.
// Double-free — ошибка программиста, не проверяется ради O(1).
//
// ПОЧЕМУ ЗАТИРАЕМ. Сам по себе free-list оставлял бы вектор лежать в va.data до
// того, как слот переиспользуют (Alloc перезаписывает ячейку целиком, так что
// после переиспользования следа нет — но «после» может не наступить никогда).
// Эмбеддинг — это содержание факта, а не его отпечаток: текст восстанавливается
// из вектора обратной моделью. Для движка, который продаёт стирание, оставлять
// удалённый факт в памяти процесса до конца её жизни — это то же самое
// расхождение между обещанием и байтами, что и незапечатанный сегмент, только
// в RAM: кордамп, своп и чтение /proc/<pid>/mem его показывают.
//
// На диск это не попадало и раньше: снапшот пишет только живые векторы
// (leveled_store.go), а сырую арену сериализует лишь VectorStore.SaveBinary —
// путь не-VMEM. То есть правка закрывает экспозицию в памяти процесса, а не
// в файлах.
//
// Цена измерена, а не оценена: BenchmarkGraphDeleteUpsertChurn 1.083 → 1.034
// мс/оп, то есть В ПРЕДЕЛАХ ШУМА (после правки даже «быстрее» — значит
// разницы нет). Иначе и не могло быть: затирание это линейная запись по dim
// элементам против полного обхода графа с прунингом соседей, который тот же
// Delete делает строкой ниже.
func (va *VectorArena) Free(offset uint64) {
	clear(va.data[offset : offset+uint64(va.dim)])
	va.freeOffsets = append(va.freeOffsets, offset)
}

// Get возвращает быстрый слайс-взгляд на вектор по смещению
func (va *VectorArena) Get(offset uint64) []float32 {
	return va.data[offset : offset+uint64(va.dim)]
}
