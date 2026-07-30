package auditchain

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func leafN(i int) Leaf {
	return Leaf{
		UnixNano: int64(1_753_700_000_000_000_000 + i),
		Type:     EventRemember,
		Scope:    "user:nikolay",
		Subject:  fmt.Sprintf("fact-%04d", i),
		Payload:  []byte("sha256:отпечаток содержания"),
	}
}

func leavesN(n int) []Leaf {
	out := make([]Leaf, n)
	for i := range out {
		out[i] = leafN(i)
	}
	return out
}

// TestMerkle_ProofVerifiesForEveryLeaf — размеры взяты нечётные специально:
// нечётный уровень поднимается без пары, и путь для последнего листа короче,
// чем для остальных. Именно здесь ломаются наивные реализации.
func TestMerkle_ProofVerifiesForEveryLeaf(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 7, 8, 9, 100, 532} {
		leaves := leavesN(n)
		root := MerkleRoot(leaves)
		for i := 0; i < n; i++ {
			path, err := MerkleProof(leaves, i)
			if err != nil {
				t.Fatalf("n=%d лист %d: %v", n, i, err)
			}
			if !VerifyProof(leaves[i], path, root) {
				t.Fatalf("n=%d: путь для листа %d не сошёлся с корнем", n, i)
			}
		}
	}
}

// TestMerkle_ProofRejectsForeignLeaf — доказательство обязано быть про
// КОНКРЕТНЫЙ лист, иначе оно доказывает лишь «батч существовал».
func TestMerkle_ProofRejectsForeignLeaf(t *testing.T) {
	leaves := leavesN(8)
	root := MerkleRoot(leaves)
	path, err := MerkleProof(leaves, 3)
	if err != nil {
		t.Fatal(err)
	}

	// ПАРНЫЙ КОНТРОЛЬ: тот же путь со СВОИМ листом обязан сходиться, иначе
	// проверка ниже прошла бы и на сломанном VerifyProof.
	if !VerifyProof(leaves[3], path, root) {
		t.Fatal("путь не сошёлся со своим листом — тест ниже проверял бы не то")
	}

	forged := leaves[3]
	forged.Subject = "fact-подменённый"
	if VerifyProof(forged, path, root) {
		t.Fatal("путь сошёлся с подменённым листом")
	}
	if VerifyProof(leaves[4], path, root) {
		t.Fatal("путь от листа 3 принял лист 4")
	}
}

// TestMerkle_SiblingSideMatters — если сторону соседа не учитывать,
// перестановка листьев перестаёт менять корень, и «доказательство» становится
// доказательством мультимножества, а не последовательности.
func TestMerkle_SiblingSideMatters(t *testing.T) {
	leaves := leavesN(4)
	root := MerkleRoot(leaves)
	path, err := MerkleProof(leaves, 1)
	if err != nil {
		t.Fatal(err)
	}
	flipped := make([]ProofStep, len(path))
	copy(flipped, path)
	for i := range flipped {
		flipped[i].SiblingLeft = !flipped[i].SiblingLeft
	}
	if VerifyProof(leaves[1], flipped, root) {
		t.Fatal("путь сошёлся при перевёрнутой стороне соседа — порядок склейки не проверяется")
	}
}

// TestMerkle_NodeCannotPassAsLeaf — ловушка 1: второй прообраз.
//
// Без разделения доменов хеш внутреннего узла лежит в одном пространстве с
// хешем листа, и узел можно предъявить как событие: путь сойдётся к тому же
// корню, доказав операцию, которой не было.
func TestMerkle_NodeCannotPassAsLeaf(t *testing.T) {
	a, b := LeafHash(leafN(0)), LeafHash(leafN(1))
	node := nodeHash(a, b)

	// ⚠Проверяются САМИ ПРЕОБРАЗОВАНИЯ, а не равенство констант. Первая
	// версия этого теста сравнивала domainLeaf с domainNode и пропустила
	// мутацию «узел хеширует себя доменом листа»: константы-то остались
	// разными, разъехалось их ПРИМЕНЕНИЕ. Поймано мутационным прогоном.
	bare := sha256.Sum256(append(append([]byte{}, a[:]...), b[:]...))
	if node == bare {
		t.Fatal("узел хешируется как голая склейка детей — домены не разделены, узел неотличим от листа")
	}
	leafLike := sha256.Sum256(append([]byte{domainLeaf}, append(append([]byte{}, a[:]...), b[:]...)...))
	if node == leafLike {
		t.Fatal("узел хешируется доменом ЛИСТА — внутренний узел можно предъявить как событие")
	}

	// Лист тоже обязан нести свой домен: иначе разделение одностороннее.
	body := encodeLeafForHash(leafN(0))
	if body[0] != domainLeaf {
		t.Fatalf("лист хешируется без домена: первый байт 0x%02x", body[0])
	}
}

// TestMerkle_OddLevelIsNotMalleable — ловушка 2: приём «продублировать
// последний узел» даёт двум РАЗНЫМ наборам листьев один корень
// (CVE-2012-2459). Тогда «в цепи ровно эти три события» и «в цепи эти четыре»
// становятся неразличимы, то есть в доказанный батч можно дописать событие.
func TestMerkle_OddLevelIsNotMalleable(t *testing.T) {
	three := leavesN(3)
	four := append(leavesN(3), leafN(2)) // последний лист продублирован

	if MerkleRoot(three) == MerkleRoot(four) {
		t.Fatal("корень над [a b c] совпал с корнем над [a b c c] — нечётный уровень дублируется")
	}
}

// TestMerkle_RootChangesOnAnyLeafEdit — базовое свойство: корень обязан
// зависеть от каждого листа целиком.
func TestMerkle_RootChangesOnAnyLeafEdit(t *testing.T) {
	leaves := leavesN(7)
	root := MerkleRoot(leaves)

	for i := range leaves {
		for _, mut := range []struct {
			name  string
			apply func(*Leaf)
		}{
			{"subject", func(l *Leaf) { l.Subject += "!" }},
			{"scope", func(l *Leaf) { l.Scope = "user:другой" }},
			{"payload", func(l *Leaf) { l.Payload = []byte("другой отпечаток") }},
			{"type", func(l *Leaf) { l.Type = EventForget }},
			{"time", func(l *Leaf) { l.UnixNano++ }},
		} {
			edited := leavesN(7)
			mut.apply(&edited[i])
			if MerkleRoot(edited) == root {
				t.Errorf("правка %s в листе %d не изменила корень", mut.name, i)
			}
		}
	}
}

// TestMerkle_FieldsAreUnambiguous — дыра 1 из шапки пакета в применении к
// листьям: без префикса длины scope="a", subject="bc" неотличим от
// scope="ab", subject="c".
func TestMerkle_FieldsAreUnambiguous(t *testing.T) {
	x := Leaf{UnixNano: 1, Type: EventRemember, Scope: "a", Subject: "bc"}
	y := Leaf{UnixNano: 1, Type: EventRemember, Scope: "ab", Subject: "c"}
	if LeafHash(x) == LeafHash(y) {
		t.Fatal("границы полей неоднозначны: перенос символа между scope и subject не меняет хеш")
	}
}

// TestMerkle_ProofRejectsOutOfRange — путь для несуществующего листа.
func TestMerkle_ProofRejectsOutOfRange(t *testing.T) {
	leaves := leavesN(4)
	for _, idx := range []int{-1, 4, 99} {
		if _, err := MerkleProof(leaves, idx); err == nil {
			t.Errorf("путь для листа %d вне батча построен", idx)
		}
	}
}
