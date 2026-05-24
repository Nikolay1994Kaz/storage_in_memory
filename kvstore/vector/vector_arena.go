package vector

// ============================================================================
// 1. VectorArena — Арена для хранения координат векторов (float32)
// ============================================================================

type VectorArena struct {
	data        []float32 // Единый плоский массив координат
	dim         int       // Размерность вектора (например, 128)
	freeOffsets []uint32  // Стек свободных смещений (Free List)
}

func NewVectorArena(dim int, initialCapacity int) *VectorArena {
	return &VectorArena{
		data:        make([]float32, 0, initialCapacity*dim),
		dim:         dim,
		freeOffsets: make([]uint32, 0, 64),
	}
}

// Allocate выделяет память под новый вектор.
//
// Паникует если len(vec) != dim — несоответствие размерности
// ломает весь layout арены (тихая порча данных).
func (va *VectorArena) Allocate(vec []float32) uint32 {
	if len(vec) != va.dim {
		panic("VectorArena.Allocate: dimension mismatch")
	}

	nFree := len(va.freeOffsets)
	if nFree > 0 {
		offset := va.freeOffsets[nFree-1]
		va.freeOffsets = va.freeOffsets[:nFree-1]
		copy(va.data[offset:offset+uint32(va.dim)], vec)
		return offset
	}

	offset := uint32(len(va.data))
	va.data = append(va.data, vec...)
	return offset
}

// Free освобождает ячейку вектора.
//
// Контракт: вызывающий код гарантирует, что offset не освобождается дважды.
// Double-free — ошибка программиста, не проверяется ради O(1).
func (va *VectorArena) Free(offset uint32) {
	va.freeOffsets = append(va.freeOffsets, offset)
}

// Get возвращает быстрый слайс-взгляд на вектор по смещению
func (va *VectorArena) Get(offset uint32) []float32 {
	return va.data[offset : offset+uint32(va.dim)]
}

// ============================================================================
// 2. NeighborsArena — Арена для списков соседей (uint64)
// ============================================================================
//
// Каждая нода получает один непрерывный блок, в котором хранятся
// списки соседей для ВСЕХ её слоёв. Формат блока:
//
//	┌───────────────────────┐
//	│ [len0] [n0_1] ... [n0_M0] │  ← Слой 0: 1 + M0 ячеек
//	│ [len1] [n1_1] ... [n1_M]  │  ← Слой 1: 1 + M ячеек
//	│ ...                       │
//	│ [lenL] [nL_1] ... [nL_M]  │  ← Слой L: 1 + M ячеек
//	└───────────────────────┘
//
// [lenX] — текущее количество соседей на слое X (length prefix).
// Остальные ячейки — ID соседей.

type NeighborsArena struct {
	data        []uint64         // Плоский массив для ID соседей
	M           int              // Максимум соседей на слоях > 0
	M0          int              // Максимум соседей на слое 0
	freeOffsets map[int][]uint32 // Карта свободных смещений, разделенная по уровням!
}

func NewNeighborsArena(M, M0 int, initialCapacity int) *NeighborsArena {
	return &NeighborsArena{
		data:        make([]uint64, 0, initialCapacity*(M+M0)),
		M:           M,
		M0:          M0,
		freeOffsets: make(map[int][]uint32),
	}
}

// blockSize возвращает размер блока в uint64 для ноды определенного уровня (О(1) версия)
func (na *NeighborsArena) blockSize(level int) int {
	if level == 0 {
		return 1 + na.M0
	}
	return 1 + na.M0 + level*(1+na.M)
}

// Allocate выделяет память под списки связей новой ноды
func (na *NeighborsArena) Allocate(level int) uint32 {
	size := na.blockSize(level)

	// Проверяем, есть ли освобожденный блок ТОЧНО такого же уровня (размера)
	if list, ok := na.freeOffsets[level]; ok && len(list) > 0 {
		offset := list[len(list)-1]
		na.freeOffsets[level] = list[:len(list)-1]

		// Зануляем старый блок перед переиспользованием
		for i := 0; i < size; i++ {
			na.data[int(offset)+i] = 0
		}
		return offset
	}

	offset := uint32(len(na.data))
	zeroes := make([]uint64, size)
	na.data = append(na.data, zeroes...)
	return offset
}

// Free освобождает блок памяти ноды и отправляет его во Free List нужного уровня.
//
// Контракт: вызывающий код гарантирует, что offset не освобождается дважды.
func (na *NeighborsArena) Free(offset uint32, level int) {
	na.freeOffsets[level] = append(na.freeOffsets[level], offset)
}

// offsetForLevel вычисляет индекс начала секции соседей конкретного слоя внутри блока ноды (О(1) версия без циклов!)
func (na *NeighborsArena) offsetForLevel(nodeOffset uint32, targetLevel int) uint32 {
	if targetLevel == 0 {
		return nodeOffset
	}
	return nodeOffset + 1 + uint32(na.M0) + uint32(targetLevel-1)*uint32(1+na.M)
}

// GetNeighbors возвращает срез соседей на конкретном слое.
//
// Возвращает slice-view в арену (zero-copy). Безопасно для чтения.
// Для модификации — скопировать, изменить, записать через SetNeighbors.
func (na *NeighborsArena) GetNeighbors(nodeOffset uint32, targetLevel int) []uint64 {
	secOffset := int(na.offsetForLevel(nodeOffset, targetLevel))
	length := int(na.data[secOffset])
	if length == 0 {
		return nil
	}
	return na.data[secOffset+1 : secOffset+1+length]
}

// SetNeighbors записывает соседей на конкретный слой
func (na *NeighborsArena) SetNeighbors(nodeOffset uint32, targetLevel int, neighbors []uint64) {
	secOffset := int(na.offsetForLevel(nodeOffset, targetLevel))

	// Пишем длину
	na.data[secOffset] = uint64(len(neighbors))

	// Пишем ID соседей
	if len(neighbors) > 0 {
		copy(na.data[secOffset+1:], neighbors)
	}
}
