package auditchain

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// closeHard закрывает файлы БЕЗ флаша — модель отказа питания. Обычный Close
// буфер сбрасывает, поэтому через него ни один из сценариев ниже не
// воспроизводится.
func closeHard(c *Carrier) {
	for _, f := range []*os.File{c.leaves, c.chain, c.head} {
		if f != nil {
			f.Close()
		}
	}
	c.leaves, c.chain, c.head = nil, nil, nil
}

func mustOpen(t *testing.T, dir string) *Carrier {
	t.Helper()
	c, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// verifyOnDisk читает цепь с диска и сверяет её с головой — то же, что сделает
// будущая команда VERIFY.
func verifyOnDisk(t *testing.T, dir string, want Head) {
	t.Helper()
	links, err := ReadChain(dir)
	if err != nil {
		t.Fatalf("ReadChain: %v", err)
	}
	if _, err := Verify(links, &want); err != nil {
		t.Fatalf("цепь на диске не сходится с головой: %v", err)
	}
}

func TestCarrier_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)

	for i := 0; i < 5; i++ {
		c.Append(leafN(i))
	}
	if c.Pending() != 5 {
		t.Fatalf("в буфере %d событий, ожидалось 5", c.Pending())
	}
	head, err := c.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if head.Seq != 1 {
		t.Fatalf("пять событий дали seq=%d, а батч обязан быть ОДНИМ звеном", head.Seq)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	verifyOnDisk(t, dir, head)

	c2 := mustOpen(t, dir)
	defer c2.Close()
	if c2.Head() != head {
		t.Fatalf("после перезапуска голова %+v, ожидалась %+v", c2.Head(), head)
	}
	if c2.Pending() != 0 {
		t.Fatalf("после чистого закрытия в буфере %d событий", c2.Pending())
	}
}

// TestCarrier_EmptyFlushIsFree — простаивающий инстанс не должен писать
// «ничего не произошло»: на живом :6381 темп 2.2 факта в сутки, а тик — раз в
// секунду, то есть 31.5 млн пустых звеньев в год.
func TestCarrier_EmptyFlushIsFree(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	defer c.Close()

	c.Append(leafN(0))
	head, err := c.Flush()
	if err != nil {
		t.Fatal(err)
	}
	sizeChain := fileSize(t, filepath.Join(dir, chainFileName))
	sizeLeaves := fileSize(t, leafPath(dir, 0))

	for i := 0; i < 100; i++ {
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Head(); got != head {
		t.Fatalf("сто пустых тиков сдвинули голову: %+v → %+v", head, got)
	}
	if got := fileSize(t, filepath.Join(dir, chainFileName)); got != sizeChain {
		t.Errorf("цепь выросла на пустых тиках: %d → %d Б", sizeChain, got)
	}
	if got := fileSize(t, leafPath(dir, 0)); got != sizeLeaves {
		t.Errorf("файл листьев вырос на пустых тиках: %d → %d Б", sizeLeaves, got)
	}
}

// ⭐TestCarrier_CrashBetweenLeavesAndLink — половина главного свойства
// носителя. Отказ между фазой 1 и фазой 2 оставляет листья без корня: они
// МЕНЕЕ ДОКАЗАНЫ, но не потеряны — следующее звено обязано их накрыть.
func TestCarrier_CrashBetweenLeavesAndLink(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	c.Append(leafN(1))
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}

	// Фаза 1 без фазы 2: листья durable, звена нет.
	if _, err := c.leaves.Write(frameLeaf(leafN(2))); err != nil {
		t.Fatal(err)
	}
	if err := c.leaves.Sync(); err != nil {
		t.Fatal(err)
	}
	closeHard(c)

	c2 := mustOpen(t, dir)
	defer c2.Close()
	rec := c2.Recovery()
	if rec.LeavesWithoutRoot != 1 {
		t.Fatalf("листьев без корня %d, ожидался 1", rec.LeavesWithoutRoot)
	}
	if c2.Head().Seq != 1 {
		t.Fatalf("голова уехала на seq=%d — звена не было, двигаться нечему", c2.Head().Seq)
	}

	c2.Append(leafN(3))
	head, err := c2.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if head.Seq != 2 {
		t.Fatalf("seq=%d после второго звена", head.Seq)
	}

	links, err := ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodeBatchPayload(links[1].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.FirstLeaf != 2 || p.Count != 2 {
		t.Fatalf("второе звено покрывает листья %d..%d, ожидалось 2..3 — уцелевший лист выпал из цепи",
			p.FirstLeaf, p.FirstLeaf+uint64(p.Count)-1)
	}
	// И он обязан доказываться: восстановленный лист не должен отличаться от
	// обычного ничем, кроме истории.
	leaves, err := ReadLeaves(dir, p.FirstLeaf, int(p.Count))
	if err != nil {
		t.Fatal(err)
	}
	path, err := MerkleProof(leaves, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyProof(leafN(2), path, p.Root) {
		t.Fatal("лист, переживший отказ, не доказывается путём Меркла")
	}
}

// ⭐TestCarrier_CrashBetweenLinkAndHead — вторая половина: звено на диске,
// голова не успела. Данные целы, голова догоняет, и об этом говорится вслух.
func TestCarrier_CrashBetweenLinkAndHead(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}

	// Фазы 1 и 2 без фазы 3.
	if _, err := c.leaves.Write(frameLeaf(leafN(1))); err != nil {
		t.Fatal(err)
	}
	if err := c.leaves.Sync(); err != nil {
		t.Fatal(err)
	}
	link, err := LinkBatch(c.headState, 42, c.nextLeaf, []Leaf{leafN(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.chain.Write(frameRecord(link)); err != nil {
		t.Fatal(err)
	}
	if err := c.chain.Sync(); err != nil {
		t.Fatal(err)
	}
	closeHard(c)

	c2 := mustOpen(t, dir)
	defer c2.Close()
	rec := c2.Recovery()
	if rec.HeadAdvanced != 1 {
		t.Fatalf("голова отставала на %d звеньев, ожидалось 1 — отставание не замечено", rec.HeadAdvanced)
	}
	if c2.Head().Seq != 2 {
		t.Fatalf("голова догнала до seq=%d, ожидалось 2", c2.Head().Seq)
	}
	if rec.LeavesWithoutRoot != 0 {
		t.Fatalf("листьев без корня %d — звено их покрывало", rec.LeavesWithoutRoot)
	}

	// Догон обязан быть ЗАПИСАН: иначе следующий отказ застанет то же
	// несогласованное состояние.
	c3 := mustOpen(t, dir)
	defer c3.Close()
	if got := c3.Recovery().HeadAdvanced; got != 0 {
		t.Fatalf("после восстановления голова снова отстаёт на %d — догон не записан на диск", got)
	}
}

// ⭐TestCarrier_TruncatedTailDetected — то, ради чего голова хранится отдельно
// от журнала.
func TestCarrier_TruncatedTailDetected(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	afterFirst := fileSize(t, filepath.Join(dir, chainFileName))
	c.Append(leafN(1))
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	closeHard(c)

	// ПАРНЫЙ КОНТРОЛЬ: нетронутый носитель обязан открываться, иначе тест
	// ниже проходил бы и на всегда-падающем Open.
	ok := mustOpen(t, dir)
	ok.Close()

	if err := os.Truncate(filepath.Join(dir, chainFileName), afterFirst); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrTruncatedChain) {
		t.Fatalf("обрезка хвоста дала %v, ожидалась ErrTruncatedChain", err)
	}
}

// TestCarrier_TornTailIsNotTamper — авария посреди записи не должна выглядеть
// как атака: недописанный кадр отбрасывается, целые остаются.
func TestCarrier_TornTailIsNotTamper(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	head, err := c.Flush()
	if err != nil {
		t.Fatal(err)
	}
	closeHard(c)

	f, err := os.OpenFile(filepath.Join(dir, chainFileName), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0, 0, 1, 44, 7, 7, 7}); err != nil { // заявлена длина 300, тела нет
		t.Fatal(err)
	}
	f.Close()

	c2 := mustOpen(t, dir)
	defer c2.Close()
	if got := c2.Recovery().TornTailBytes; got != 7 {
		t.Fatalf("отброшено %d Б оборванного хвоста, ожидалось 7", got)
	}
	if c2.Head() != head {
		t.Fatalf("оборванный хвост сдвинул голову: %+v", c2.Head())
	}
}

// TestCarrier_CorruptionInsideChainIsLoud — парная к предыдущей: битый кадр НЕ
// в хвосте молчанием не покрывается.
func TestCarrier_CorruptionInsideChainIsLoud(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < 2; i++ {
		c.Append(leafN(i))
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	closeHard(c)

	path := filepath.Join(dir, chainFileName)
	sizeBefore := fileSize(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff // тело ПЕРВОГО звена
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Open(dir)
	if err == nil {
		t.Fatal("порча в середине журнала принята молча")
	}
	// ⚠ПРОВЕРЯЕТСЯ ДИАГНОЗ, А НЕ ФАКТ ОШИБКИ. Первая версия теста требовала
	// лишь err != nil и пропускала мутацию «битый кадр = конец файла»: журнал
	// обрезался до нуля, и ошибка приходила от головы, не от порчи. Зелёный по
	// неверной причине; поймано мутационным прогоном.
	if errors.Is(err, ErrTruncatedChain) {
		t.Fatalf("порча в середине названа обрезкой хвоста — диагноз подменён: %v", err)
	}
	if got := fileSize(t, path); got != sizeBefore {
		t.Fatalf("носитель обрезал повреждённый журнал (%d → %d Б) — улика уничтожена при попытке её прочесть",
			sizeBefore, got)
	}
}

// TestCarrier_SecondHeadSlotSurvives — зачем слотов два: перезапись на месте
// не атомарна, и отказ питания посреди неё портит именно свежий слот.
func TestCarrier_SecondHeadSlotSurvives(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < 2; i++ {
		c.Append(leafN(i))
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	closeHard(c)

	// seq=2 лёг в слот 0 (чередование по чётности) — портим именно его.
	hp := filepath.Join(dir, headFileName)
	data, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < headSlotSize; i++ {
		data[i] ^= 0xff
	}
	if err := os.WriteFile(hp, data, 0o600); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(dir)
	if err != nil {
		t.Fatalf("порча свежего слота головы сделала носитель неоткрываемым: %v", err)
	}
	defer c2.Close()
	if c2.Head().Seq != 2 {
		t.Fatalf("голова seq=%d, ожидалось 2", c2.Head().Seq)
	}
	// Уцелел слот с seq=1 — значит носитель обязан был увидеть отставание.
	if got := c2.Recovery().HeadAdvanced; got != 1 {
		t.Fatalf("отставание головы %d, ожидалось 1", got)
	}
}

// ⭐TestCarrier_ForeignHeadRejected — «голова отстала» не должно стать
// универсальным оправданием: журнал ЧУЖОЙ цепи обязан быть отвергнут.
func TestCarrier_ForeignHeadRejected(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < 2; i++ {
		c.Append(leafN(i))
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	closeHard(c)

	// Голова с seq=1, но чужим хешем: длина сходится, содержание — нет.
	forged := encodeHead(Head{Seq: 1, Hash: [32]byte{0xde, 0xad}})
	f, err := os.OpenFile(filepath.Join(dir, headFileName), os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(forged, headSlotSize); err != nil { // слот seq=1
		t.Fatal(err)
	}
	// И затираем слот со свежей головой, чтобы читалась именно подделка.
	if _, err := f.WriteAt(make([]byte, headSlotSize), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := Open(dir); err == nil {
		t.Fatal("журнал принят при голове от другой цепи")
	}
}

// ⭐TestCarrier_AppendSyncClosesWindow — форс-флаш на доказываемых событиях.
//
// Смысл не в скорости SHRED, а в том, что его квитанция обязана покрывать ВСЁ,
// что было раньше. Если события, накопленные до него, останутся в буфере, окно
// недоказуемости накроет ровно тот момент, который продаётся как доказанный.
func TestCarrier_AppendSyncClosesWindow(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	defer c.Close()

	c.Append(leafN(0))
	c.Append(leafN(1))

	shred := Leaf{
		UnixNano: 1_753_700_000_000_000_099,
		Type:     EventShred,
		Scope:    "user:nikolay",
		Payload:  []byte(`{"kek_id":"k1","facts_removed":7}`),
	}
	head, err := c.AppendSync(shred)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pending() != 0 {
		t.Fatalf("после синхронного события в буфере осталось %d — окно не закрыто", c.Pending())
	}
	if head.Seq != 1 {
		t.Fatalf("seq=%d", head.Seq)
	}

	links, err := ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodeBatchPayload(links[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.Count != 3 || p.FirstLeaf != 0 {
		t.Fatalf("звено покрывает %d листьев с %d, ожидалось 3 с 0 — события до квитанции не покрыты",
			p.Count, p.FirstLeaf)
	}
}

// ⭐TestCarrier_ProofFromDisk — сквозной путь квитанции: звено даёт корень,
// файл листьев даёт лист, путь Меркла связывает их. Аудитору не нужна вся
// цепь и не видны чужие события.
func TestCarrier_ProofFromDisk(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	const n = 37
	for i := 0; i < n; i++ {
		c.Append(leafN(i))
	}
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	links, err := ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodeBatchPayload(links[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	leaves, err := ReadLeaves(dir, p.FirstLeaf, int(p.Count))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != n {
		t.Fatalf("с диска прочитано %d листьев, записано %d", len(leaves), n)
	}
	for i := 0; i < n; i++ {
		if LeafHash(leaves[i]) != LeafHash(leafN(i)) {
			t.Fatalf("лист %d прочитан с диска не таким, каким записан", i)
		}
		path, err := MerkleProof(leaves, i)
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(leaves[i], path, p.Root) {
			t.Fatalf("лист %d не доказывается корнем из звена", i)
		}
	}
}

var errBoom = errors.New("отказ носителя (тест)")

// ⭐TestCarrier_OrderSurvivesFailureAtEveryPhase — ГЛАВНЫЙ тест носителя.
//
// Проверяется не «работает при отказе», а конкретное свойство порядка
// листья → звено → голова: отказ в ЛЮБОЙ точке тика обязан оставлять менее
// доказанное, но не менее доказуемое. Разрешено потерять доказательство
// последних событий; запрещено получить звено, чьих листьев нет, — оно
// навсегда означает «что-то было, а что, неизвестно».
//
// ⚠Именно этот тест краснеет при перестановке фаз, а больше не краснеет
// НИЧЕГО: при штатной работе перестановка не видна, файлы получаются те же.
func TestCarrier_OrderSurvivesFailureAtEveryPhase(t *testing.T) {
	for _, failAt := range []string{"leaves", "link", "head"} {
		t.Run(failAt, func(t *testing.T) {
			dir := t.TempDir()
			c := mustOpen(t, dir)
			c.Append(leafN(0))
			proved, err := c.Flush() // тик, который УЖЕ доказан
			if err != nil {
				t.Fatal(err)
			}

			c.beforeWrite = func(phase string) error {
				if phase == failAt {
					return errBoom
				}
				return nil
			}
			c.Append(leafN(1))
			c.Append(leafN(2))
			if _, err := c.Flush(); !errors.Is(err, errBoom) {
				t.Fatalf("отказ на фазе %q не сработал, Flush вернул %v", failAt, err)
			}
			closeHard(c)

			c2, err := Open(dir)
			if err != nil {
				t.Fatalf("после отказа на фазе %q носитель не открывается: %v", failAt, err)
			}
			defer c2.Close()

			if c2.Head().Seq < proved.Seq {
				t.Fatalf("голова откатилась с seq=%d до %d — доказанное раньше потеряно",
					proved.Seq, c2.Head().Seq)
			}

			// ⭐Инвариант: у каждого звена есть его листья.
			links, err := ReadChain(dir)
			if err != nil {
				t.Fatal(err)
			}
			for i, l := range links {
				p, err := DecodeBatchPayload(l.Payload)
				if err != nil {
					t.Fatalf("звено %d: %v", i, err)
				}
				leaves, err := ReadLeaves(dir, p.FirstLeaf, int(p.Count))
				if err != nil {
					t.Fatalf("звено %d обещает листья %d..%d, а их нет: %v — корень без листьев",
						i, p.FirstLeaf, p.FirstLeaf+uint64(p.Count)-1, err)
				}
				if MerkleRoot(leaves) != p.Root {
					t.Fatalf("звено %d: корень не сходится с листьями на диске", i)
				}
			}

			// И носитель обязан остаться рабочим, а не только целым.
			c2.Append(leafN(3))
			head, err := c2.Flush()
			if err != nil {
				t.Fatalf("после отказа на фазе %q носитель не пишет дальше: %v", failAt, err)
			}
			verifyOnDisk(t, dir, head)
		})
	}
}

// TestCarrier_Rotation — листья режутся, цепь не трогается. Это и есть
// расцепление политик хранения: корни вечны, листья по retention.
func TestCarrier_Rotation(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.segmentBytes = 1 // ротировать после каждого тика

	for i := 0; i < 3; i++ {
		c.Append(leafN(i))
		if _, err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	head := c.Head()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	segs, err := leafSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("файлов листьев %d, ожидалось 3 (по одному на тик)", len(segs))
	}

	c2 := mustOpen(t, dir)
	defer c2.Close()
	if c2.Head() != head {
		t.Fatalf("после ротации и перезапуска голова разъехалась: %+v против %+v", c2.Head(), head)
	}
	// Лист из САМОГО СТАРОГО файла обязан доказываться — иначе ротация
	// молча обесценила бы прошлые квитанции.
	leaves, err := ReadLeaves(dir, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if LeafHash(leaves[0]) != LeafHash(leafN(0)) {
		t.Fatal("лист из первого файла прочитан неверно")
	}
}

// ⭐TestCarrier_RotationWaitsForRecoveredLeaves — ротация обязана пропустить
// тик, если часть батча уже лежит на диске после восстановления. Иначе один
// батч разъедется по двум файлам, и доказательство для него не построится
// вообще: ReadLeaves читает ОДИН файл, потому что порядок листьев внутри
// батча — это и есть их позиция в дереве.
func TestCarrier_RotationWaitsForRecoveredLeaves(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	// Отказ после фазы 1: лист 1 durable, корня нет.
	if _, err := c.leaves.Write(frameLeaf(leafN(1))); err != nil {
		t.Fatal(err)
	}
	if err := c.leaves.Sync(); err != nil {
		t.Fatal(err)
	}
	closeHard(c)

	c2 := mustOpen(t, dir)
	defer c2.Close()
	if c2.Pending() != 1 {
		t.Fatalf("восстановлено %d листьев без корня, ожидался 1", c2.Pending())
	}
	c2.segmentBytes = 1 // «ротировать пора» — но батч разрывать нельзя
	c2.Append(leafN(2))
	if _, err := c2.Flush(); err != nil {
		t.Fatal(err)
	}

	links, err := ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodeBatchPayload(links[len(links)-1].Payload)
	if err != nil {
		t.Fatal(err)
	}
	leaves, err := ReadLeaves(dir, p.FirstLeaf, int(p.Count))
	if err != nil {
		t.Fatalf("батч разъехался по двум файлам листьев: %v", err)
	}
	if MerkleRoot(leaves) != p.Root {
		t.Fatal("корень звена не сходится с листьями на диске после ротации")
	}
}

// TestCarrier_CloseFlushes — штатная остановка не должна терять доказуемость
// так же, как авария.
func TestCarrier_CloseFlushes(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := mustOpen(t, dir)
	defer c2.Close()
	if c2.Head().Seq != 1 {
		t.Fatalf("Close не сбросил буфер: голова seq=%d", c2.Head().Seq)
	}
	if c2.Pending() != 0 {
		t.Fatalf("после Close остались непокрытые листья: %d", c2.Pending())
	}
}

// ⭐TestCarrier_LinkSizeFitsVolumeBudget — сторож бюджета объёма.
//
// Весь выбор структуры держится на одном числе: цепь не компактится, значит
// растёт вечно, и при тике 1 с её годовой объём равен размеру звена, умноженному
// на 31.5 млн. Порог 10 ГБ/год назначен до кода. Любое поле, добавленное в
// звено, двигает это число — и тест обязан краснеть раньше, чем диск.
//
// ⚠Здесь размер ИЗМЕРЯЕТСЯ по файлу, а не считается по структуре: расчёт
// «97 Б» из разбора 29.07 не учитывал ни кадр, ни номер первого листа.
func TestCarrier_LinkSizeFitsVolumeBudget(t *testing.T) {
	const (
		ticksPerYear   = 31_536_000 // тик 1 с — решение 5
		yearlyBudget   = 10e9       // порог, назначенный до кода
		leavesPerBatch = 532        // канонный темп на тике 100 мс, для справки
	)
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < leavesPerBatch; i++ {
		c.Append(leafN(i))
	}
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	link := fileSize(t, filepath.Join(dir, chainFileName))
	leaf := float64(fileSize(t, leafPath(dir, 0))) / leavesPerBatch
	perYear := float64(link) * ticksPerYear
	t.Logf("звено %d Б → %.2f ГБ/год при тике 1 с; лист %.0f Б", link, perYear/1e9, leaf)

	if perYear > yearlyBudget {
		t.Fatalf("звено %d Б даёт %.1f ГБ/год — порог 10 ГБ/год пробит, вернуться к выбору периода агрегации",
			link, perYear/1e9)
	}
	// Размер звена не должен зависеть от размера батча — иначе это уже не
	// агрегат, и вся экономия исчезает.
	if link > 200 {
		t.Errorf("звено %d Б: подозрительно много для агрегата из корня, счётчика и номера", link)
	}
}

// TestCarrier_RecordRoundTrip — кодирование звена обратимо. Проверяется
// отдельно, потому что разбор с диска — единственный путь к VERIFY.
func TestCarrier_RecordRoundTrip(t *testing.T) {
	head := Head{Seq: 7, Hash: [32]byte{1, 2, 3}}
	src, err := LinkBatch(head, 12345, 900, leavesN(4))
	if err != nil {
		t.Fatal(err)
	}
	body := frameRecord(src)
	got, err := decodeRecord(body[4 : len(body)-4])
	if err != nil {
		t.Fatal(err)
	}
	if Hash(got) != Hash(src) {
		t.Fatalf("звено после разбора даёт другой хеш:\n%+v\n%+v", src, got)
	}
	if got.Scope != "" {
		t.Errorf("у звена-агрегата непустой scope %q — батч охватывает разные скоупы", got.Scope)
	}
}

func TestCarrier_LeafRoundTrip(t *testing.T) {
	for _, l := range []Leaf{
		leafN(3),
		{UnixNano: 1, Type: EventShred, Scope: "s", Subject: "", Payload: nil},
		{UnixNano: -1, Type: EventForget, Scope: "", Subject: "", Payload: []byte{0, 255}},
	} {
		body := frameLeaf(l)
		got, err := decodeLeaf(body[4 : len(body)-4])
		if err != nil {
			t.Fatalf("%+v: %v", l, err)
		}
		if LeafHash(got) != LeafHash(l) {
			t.Errorf("лист после разбора даёт другой хеш: %+v против %+v", got, l)
		}
	}
}
