package auditchain

import (
	"crypto/sha256"
	"fmt"
)

// Дерево Меркла над батчем листьев.
//
// ЗАЧЕМ ОНО ЗДЕСЬ. Цепь платит объёмом за каждую запись и не может быть
// сжата. Дерево меняет «запись на событие» на «запись на тик»: в цепь идёт
// один корень, события лежат листьями рядом. Доказательство того, что
// конкретный факт в цепи есть, становится путём длиной log₂N — при тысяче
// событий в батче это десять хешей, около 350 Б.
//
// ⭐ПОБОЧНОЕ СВОЙСТВО, КОТОРОЕ ВАЖНЕЕ ЭКОНОМИИ. Путь Меркла доказывает
// ВКЛЮЧЕНИЕ СВОЕГО листа, не раскрывая соседних. Владелец памяти может
// доказать «моя запись в журнале есть и не менялась», не показывая аудитору
// ничего про других людей в том же батче. Прямая запись такого не умеет:
// чтобы проверить цепь, её надо прочесть целиком.
//
// ⚠ДВЕ ИЗВЕСТНЫЕ ЛОВУШКИ ДЕРЕВЬЕВ МЕРКЛА, ОБОЙДЁННЫЕ ПО ПОСТРОЕНИЮ.
//
//  1. ВТОРОЙ ПРООБРАЗ. Если лист и внутренний узел хешируются одинаково,
//     внутренний узел можно предъявить как лист: путь сойдётся к тому же
//     корню, и «доказательство» будет верным для события, которого не было.
//     Лечится разделением доменов — лист хешируется с префиксом 0x00, узел с
//     0x01, и пространства не пересекаются.
//  2. МАЛЛЕАБИЛЬНОСТЬ НЕЧЁТНОГО УРОВНЯ. Классический приём «продублировать
//     последний узел» даёт двум РАЗНЫМ наборам листьев один корень
//     (CVE-2012-2459 в биткойне). Здесь нечётный узел поднимается на уровень
//     выше БЕЗ изменения — дублирования нет, значит нет и подмены.
const (
	domainLeaf byte = 0x00
	domainNode byte = 0x01
)

// LeafHash — хеш листа.
func LeafHash(l Leaf) [32]byte {
	return sha256.Sum256(encodeLeafForHash(l))
}

// nodeHash — хеш внутреннего узла из двух детей.
func nodeHash(left, right [32]byte) [32]byte {
	var buf [1 + 2*hashSize]byte
	buf[0] = domainNode
	copy(buf[1:1+hashSize], left[:])
	copy(buf[1+hashSize:], right[:])
	return sha256.Sum256(buf[:])
}

// MerkleRoot — корень над листьями в порядке их добавления.
//
// Пустой батч даёт нулевой корень; звено над ним не строится (см. LinkBatch),
// поэтому наружу это значение не выходит.
func MerkleRoot(leaves []Leaf) [32]byte {
	if len(leaves) == 0 {
		return [32]byte{}
	}
	level := make([][32]byte, len(leaves))
	for i, l := range leaves {
		level[i] = LeafHash(l)
	}
	for len(level) > 1 {
		level = nextLevel(level)
	}
	return level[0]
}

// nextLevel сворачивает уровень попарно. Нечётный последний узел поднимается
// как есть — см. ловушку 2 в шапке файла.
func nextLevel(level [][32]byte) [][32]byte {
	next := make([][32]byte, 0, (len(level)+1)/2)
	for i := 0; i < len(level); i += 2 {
		if i+1 == len(level) {
			next = append(next, level[i])
			continue
		}
		next = append(next, nodeHash(level[i], level[i+1]))
	}
	return next
}

// ProofStep — одно звено пути от листа к корню.
//
// SiblingLeft говорит, с какой стороны стоит сосед. Без этого флага порядок
// склейки пришлось бы угадывать, а nodeHash(a,b) ≠ nodeHash(b,a) — и проверка
// либо разошлась бы, либо (хуже) стала бы принимать перестановку листьев.
type ProofStep struct {
	Hash        [32]byte
	SiblingLeft bool
}

// MerkleProof строит путь включения листа с индексом idx.
func MerkleProof(leaves []Leaf, idx int) ([]ProofStep, error) {
	if idx < 0 || idx >= len(leaves) {
		return nil, fmt.Errorf("auditchain: лист %d вне батча из %d", idx, len(leaves))
	}
	level := make([][32]byte, len(leaves))
	for i, l := range leaves {
		level[i] = LeafHash(l)
	}

	var path []ProofStep
	pos := idx
	for len(level) > 1 {
		// Одинокий последний узел поднимается без соседа — шага в пути нет.
		if pos == len(level)-1 && len(level)%2 == 1 {
			level = nextLevel(level)
			pos /= 2
			continue
		}
		if pos%2 == 0 {
			path = append(path, ProofStep{Hash: level[pos+1], SiblingLeft: false})
		} else {
			path = append(path, ProofStep{Hash: level[pos-1], SiblingLeft: true})
		}
		level = nextLevel(level)
		pos /= 2
	}
	return path, nil
}

// VerifyProof проверяет, что лист входит в батч с этим корнем.
//
// Проверяющему не нужны ни остальные листья, ни сама цепь — только лист, путь
// и корень, который он взял из звена. Это и есть «докажи свой факт, не
// показывая чужих».
func VerifyProof(l Leaf, path []ProofStep, root [32]byte) bool {
	h := LeafHash(l)
	for _, step := range path {
		if step.SiblingLeft {
			h = nodeHash(step.Hash, h)
		} else {
			h = nodeHash(h, step.Hash)
		}
	}
	return h == root
}
