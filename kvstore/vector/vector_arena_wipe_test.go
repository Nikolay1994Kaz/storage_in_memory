package vector

import (
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// Затирание освобождённой ячейки арены.
//
// ЧТО ЭТО ЗАКРЫВАЕТ. Free клал offset в free-list и оставлял вектор лежать в
// va.data до переиспользования слота — а оно может не наступить никогда.
// Эмбеддинг это содержание факта, а не отпечаток: текст восстанавливается из
// вектора обратной моделью. Для движка, продающего стирание, удалённый факт,
// живущий в памяти процесса до её конца, — то же расхождение обещания с
// байтами, что и незапечатанный сегмент, только в RAM.
//
// Экспозиция была именно в памяти: снапшот пишет только живые векторы, сырую
// арену сериализует лишь VectorStore.SaveBinary (путь не-VMEM).

func TestArenaFreeWipesSlot(t *testing.T) {
	const dim = 8
	va := NewVectorArena(dim, 4)

	vec := []float32{1, 2, 3, 4, 5, 6, 7, 8}
	off := va.Allocate(vec)

	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: до Free вектор обязан лежать на месте.
	// Без него проверка «после Free одни нули» прошла бы и на арене, куда
	// вообще ничего не записалось.
	got := va.Get(off)
	for i := range vec {
		if got[i] != vec[i] {
			t.Fatalf("до Free ячейка[%d] = %v, ожидалось %v — записи не было, проверка ниже бессмысленна",
				i, got[i], vec[i])
		}
	}

	va.Free(off)

	// Читаем НАПРЯМУЮ по смещению, а не через Get: смысл проверки в том, что
	// байт в памяти больше нет, а не в том, что их не отдаёт аксессор.
	for i := 0; i < dim; i++ {
		if v := va.data[off+uint64(i)]; v != 0 {
			t.Errorf("после Free ячейка[%d] = %v, ожидался 0 — вектор удалённого факта остался в памяти процесса", i, v)
		}
	}
}

// TestArenaReuseAfterWipe — затирание не должно ломать переиспользование слота:
// освобождённая ячейка обязана снова принимать данные.
func TestArenaReuseAfterWipe(t *testing.T) {
	const dim = 4
	va := NewVectorArena(dim, 4)

	first := []float32{9, 9, 9, 9}
	off := va.Allocate(first)
	va.Free(off)

	second := []float32{1, 2, 3, 4}
	reused := va.Allocate(second)
	if reused != off {
		t.Fatalf("слот не переиспользован: получено смещение %d, ожидалось %d — free-list сломан", reused, off)
	}
	got := va.Get(reused)
	for i := range second {
		if got[i] != second[i] {
			t.Errorf("после переиспользования ячейка[%d] = %v, ожидалось %v", i, got[i], second[i])
		}
	}
}

// TestGraphDeleteWipesVector — та же гарантия на уровне графа: именно этим
// путём удаляются факты, и именно он должен не оставлять следа.
func TestGraphDeleteWipesVector(t *testing.T) {
	const dim = 8
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	g.arena = NewVectorArena(dim, 4)

	vec := []float32{11, 22, 33, 44, 55, 66, 77, 88}
	id := g.Insert(vec)

	off := g.nodes[id].VectorOffset
	// Парный контроль: вектор на месте до удаления.
	if g.arena.data[off] != vec[0] {
		t.Fatalf("вектор не записан в арену — проверка ниже прошла бы по неверной причине")
	}

	if !g.Delete(uint64(id)) {
		t.Fatal("Delete вернул false — узел не удалён, проверять нечего")
	}
	for i := 0; i < dim; i++ {
		if v := g.arena.data[off+uint64(i)]; v != 0 {
			t.Errorf("после Delete ячейка[%d] = %v, ожидался 0 — вектор удалённого факта остался в памяти", i, v)
		}
	}
}
