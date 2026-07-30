package auditchain

import (
	"os"
	"path/filepath"
	"testing"
)

// chainOfLinks — n звеньев по одному листу в каждом.
func chainOfLinks(t *testing.T, dir string, n int) Head {
	t.Helper()
	c := mustOpen(t, dir)
	for i := 0; i < n; i++ {
		c.Append(leafN(i))
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	head := c.Head()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return head
}

func TestVerifyRange_FullPassMatchesHead(t *testing.T) {
	dir := t.TempDir()
	head := chainOfLinks(t, dir, 6)

	got, checked, err := VerifyRange(dir, 0, &head)
	if err != nil {
		t.Fatalf("полный проход отверг целую цепь: %v", err)
	}
	if got != head || checked != 6 {
		t.Fatalf("проверено %d звеньев, голова %+v", checked, got)
	}
}

// ⭐TestVerifyRange_FromOporаCatchesLaterTampering — окно обязано ловить
// правку ПОСЛЕ опоры. То, что до неё, оно не проверяет по построению, и это
// названо: аудитор опирается на прошлое подписанное заявление.
func TestVerifyRange_FromCatchesLaterTampering(t *testing.T) {
	dir := t.TempDir()
	head := chainOfLinks(t, dir, 6)

	// ПАРНЫЙ КОНТРОЛЬ: на целой цепи сверка с середины проходит.
	if _, checked, err := VerifyRange(dir, 3, &head); err != nil || checked != 3 {
		t.Fatalf("целая цепь не прошла сверку с звена 3: checked=%d err=%v", checked, err)
	}

	// Портим ПЯТОЕ звено — оно внутри окна.
	path := filepath.Join(dir, chainFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := len(data) / 6
	data[4*frame+20] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyRange(dir, 3, &head); err == nil {
		t.Fatal("правка внутри окна не обнаружена")
	}
}

// ⭐TestVerifyRange_CatchesRelinkedChain — правка, СОХРАНЯЮЩАЯ кадры валидными.
//
// ⚠Тест выше портил байт и был зелёным по неверной причине: ошибку возвращал
// разбор кадра по CRC, а сама проверка связи PrevHash оставалась непокрытой.
// Мутация «не сверять PrevHash» проходила насквозь. Здесь звено
// ПЕРЕСОБИРАЕТСЯ с корректным CRC и корректным seq — ровно то, что сделает
// тот, кто переписывает прошлое, а не тот, у кого побился диск.
func TestVerifyRange_CatchesRelinkedChain(t *testing.T) {
	dir := t.TempDir()
	head := chainOfLinks(t, dir, 6)

	links, err := ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Пятое звено получает ЧУЖОГО предка. Seq не трогаем — иначе упадёт
	// проверка пропуска, и снова окажется, что проверяли не то.
	links[4].PrevHash[0] ^= 0xff

	var buf []byte
	for _, l := range links {
		buf = append(buf, frameRecord(l)...)
	}
	if err := os.WriteFile(filepath.Join(dir, chainFileName), buf, 0o600); err != nil {
		t.Fatal(err)
	}

	// ПАРНЫЙ КОНТРОЛЬ: кадры остались валидными — ReadChain не жалуется.
	if _, err := ReadChain(dir); err != nil {
		t.Fatalf("пересобранная цепь не читается, значит тест проверяет разбор, а не связность: %v", err)
	}
	if _, _, err := VerifyRange(dir, 3, &head); err == nil {
		t.Fatal("разорванная связь PrevHash не обнаружена при сверке с опоры")
	}
	if _, _, err := VerifyRange(dir, 0, &head); err == nil {
		t.Fatal("разорванная связь PrevHash не обнаружена при полном проходе")
	}
}

func TestVerifyRange_RejectsImpossibleStart(t *testing.T) {
	dir := t.TempDir()
	chainOfLinks(t, dir, 3)
	if _, _, err := VerifyRange(dir, 99, nil); err == nil {
		t.Fatal("сверка с несуществующего звена принята")
	}
}

// ⭐TestForEachLeaf_ReportsRetentionHonestly — листья живут по retention,
// звенья вечны. Обход обязан сказать, сколько журнала УЖЕ НЕТ: иначе сверка
// объявит ранние факты незаписанными и обвинит систему в порче, которой не
// было.
func TestForEachLeaf_ReportsRetentionHonestly(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.segmentBytes = 1 // каждый тик — свой файл листьев
	for i := 0; i < 4; i++ {
		c.Append(leafN(i))
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()

	// ПАРНЫЙ КОНТРОЛЬ: пока файлы на месте, видно всё.
	var seen int
	cov, err := ForEachLeaf(dir, func(Leaf) { seen++ })
	if err != nil {
		t.Fatal(err)
	}
	if seen != 4 || cov.LeavesRead != 4 || cov.LeavesExpired != 0 {
		t.Fatalf("до retention: прочитано %d, cov=%+v", seen, cov)
	}

	// Retention выбросил самый старый файл листьев.
	segs, err := leafSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(leafPath(dir, segs[0])); err != nil {
		t.Fatal(err)
	}

	seen = 0
	cov, err = ForEachLeaf(dir, func(Leaf) { seen++ })
	if err != nil {
		t.Fatalf("истёкшие листья приняты за поломку: %v", err)
	}
	if cov.LeavesExpired == 0 {
		t.Fatal("об истёкших листьях не сказано — сверка сочтёт эти факты незаписанными")
	}
	if cov.LeavesRead+int(cov.LeavesExpired) != 4 {
		t.Fatalf("прочитано %d + истекло %d ≠ 4", cov.LeavesRead, cov.LeavesExpired)
	}
	if cov.Links != 4 {
		t.Fatalf("звеньев %d, ожидалось 4 — звенья retention не трогает", cov.Links)
	}
}

// ⭐TestForEachLeaf_LoudOnCorruptLeafFile — порча ФАЙЛА ЛИСТЬЕВ не должна
// сойти за retention.
//
// ⚠Первая версия этого набора портила chain.log и была зелёной по неверной
// причине: ошибку возвращал ReadChain, а ветка «листья истекли» оставалась
// непроверенной. Мутация «считать любую ошибку чтения листьев за retention»
// прошла насквозь — и это самая опасная из возможных: она превращает порчу
// улики в штатное сообщение «данные истекли».
func TestForEachLeaf_LoudOnCorruptLeafFile(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < 3; i++ {
		c.Append(leafN(i))
	}
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	c.Close()

	// ПАРНЫЙ КОНТРОЛЬ: до порчи листья читаются.
	var seen int
	if cov, err := ForEachLeaf(dir, func(Leaf) { seen++ }); err != nil || seen != 3 || cov.LeavesExpired != 0 {
		t.Fatalf("до порчи: seen=%d cov=%+v err=%v", seen, cov, err)
	}

	// Портим тело кадра ВНУТРИ файла листьев, не трогая цепь.
	path := leafPath(dir, 0)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[12] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cov, err := ForEachLeaf(dir, func(Leaf) {})
	if err == nil {
		t.Fatalf("порча файла листьев принята молча (cov=%+v)", cov)
	}
	if cov.LeavesExpired != 0 {
		t.Fatalf("порча засчитана как истёкшие по retention листья: %+v", cov)
	}
}

func TestForEachLeaf_LoudOnRealCorruption(t *testing.T) {
	dir := t.TempDir()
	chainOfLinks(t, dir, 3)

	// Порча ЗВЕНА — не retention, молчать нельзя.
	path := filepath.Join(dir, chainFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ForEachLeaf(dir, func(Leaf) {}); err == nil {
		t.Fatal("порча цепи принята молча")
	}
}
