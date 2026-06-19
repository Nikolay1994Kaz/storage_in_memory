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

// Free освобождает ячейку вектора.
//
// Контракт: вызывающий код гарантирует, что offset не освобождается дважды.
// Double-free — ошибка программиста, не проверяется ради O(1).
func (va *VectorArena) Free(offset uint64) {
	va.freeOffsets = append(va.freeOffsets, offset)
}

// Get возвращает быстрый слайс-взгляд на вектор по смещению
func (va *VectorArena) Get(offset uint64) []float32 {
	return va.data[offset : offset+uint64(va.dim)]
}
