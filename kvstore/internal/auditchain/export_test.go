package auditchain

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func newSigner(t *testing.T) (*Signer, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := LoadOrCreateSigner(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSigner: %v", err)
	}
	return s, dir
}

func TestSigner_KeyIsStableAcrossRestarts(t *testing.T) {
	s, dir := newSigner(t)
	again, err := LoadOrCreateSigner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicKeyString() != again.PublicKeyString() {
		t.Fatal("после перезапуска ключ другой — все прежде выданные заявления перестали бы сходиться")
	}

	// Приватный ключ не должен быть читаем никому, кроме владельца процесса.
	st, err := os.Stat(filepath.Join(dir, SignKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("права на ключ подписи %o, ожидалось 600", perm)
	}
}

func TestStatement_VerifiesAndDetectsEdits(t *testing.T) {
	s, _ := newSigner(t)
	head := Head{Seq: 42, Hash: [32]byte{1, 2, 3}}
	st := s.Sign(head, 42, 1_753_700_000)

	if err := VerifyStatement(st, s.PublicKey()); err != nil {
		t.Fatalf("своё заявление отвергнуто: %v", err)
	}

	// Каждое подписанное поле обязано быть подписанным на самом деле.
	for _, mut := range []struct {
		name  string
		apply func(*Statement)
	}{
		{"head_seq", func(x *Statement) { x.HeadSeq++ }},
		{"head_hash", func(x *Statement) { x.HeadHash = "AAAA" }},
		{"links", func(x *Statement) { x.Links = 1 }},
		{"signed_at", func(x *Statement) { x.SignedAt++ }},
		{"version", func(x *Statement) { x.Version = 99 }},
	} {
		edited := st
		mut.apply(&edited)
		if err := VerifyStatement(edited, s.PublicKey()); err == nil {
			t.Errorf("правка поля %s не обнаружена — поле подписью не покрыто", mut.name)
		}
	}
}

// ⭐TestStatement_ForeignInstanceIsRejected — то, ради чего подпись
// асимметричная. Аудитор, однажды закрепивший публичный ключ, обязан заметить
// подмену сервера или «нового чистого» журнала.
func TestStatement_ForeignInstanceIsRejected(t *testing.T) {
	mine, _ := newSigner(t)
	theirs, _ := newSigner(t)

	head := Head{Seq: 7, Hash: [32]byte{9}}
	forged := theirs.Sign(head, 7, 1_753_700_000)

	// ПАРНЫЙ КОНТРОЛЬ: чужое заявление внутренне безупречно и без
	// закреплённого ключа проходит проверку. Именно поэтому nil — не
	// доказательство происхождения, и это записано в доке функции.
	if err := VerifyStatement(forged, nil); err != nil {
		t.Fatalf("внутренне корректное заявление отвергнуто без закреплённого ключа: %v", err)
	}
	if err := VerifyStatement(forged, mine.PublicKey()); err == nil {
		t.Fatal("заявление чужого инстанса принято при закреплённом ключе")
	}
}

func TestStatement_RoundTripsThroughJSON(t *testing.T) {
	s, _ := newSigner(t)
	st := s.Sign(Head{Seq: 5, Hash: [32]byte{7}}, 5, 1_753_700_000)
	b, err := st.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseStatement(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyStatement(got, s.PublicKey()); err != nil {
		t.Fatalf("заявление не пережило JSON: %v", err)
	}
}

// ⭐TestProof_EndToEnd — доказательство включения, каким его увидит аудитор.
func TestProof_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < 9; i++ {
		c.Append(leafN(i))
	}
	head, err := c.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	s, _ := newSigner(t)
	st := s.Sign(head, 1, 1_753_700_000)

	link, leaves, idx, err := FindLeaf(dir, LeafQuery{
		Type: EventRemember, Scope: "user:nikolay", Subject: leafN(4).Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 4 {
		t.Fatalf("найден лист %d, ожидался 4", idx)
	}
	proof, err := BuildProof(link, leaves, idx, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.Verify(s.PublicKey()); err != nil {
		t.Fatalf("собственное доказательство не проходит проверку: %v", err)
	}

	// Через JSON — как оно и поедет аудитору.
	b, err := proof.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseProof(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Verify(s.PublicKey()); err != nil {
		t.Fatalf("доказательство не пережило JSON: %v", err)
	}

	// ⭐И оно НЕ раскрывает соседей: в документе один лист, а не батч.
	if got.Leaf.Subject != leafN(4).Subject {
		t.Fatal("в доказательстве не тот лист")
	}
	for i := 0; i < 9; i++ {
		if i == 4 {
			continue
		}
		if containsSub(string(b), leafN(i).Subject) {
			t.Fatalf("в доказательстве видно чужое событие %s — приватность батча нарушена", leafN(i).Subject)
		}
	}
}

func TestProof_DetectsEdits(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	for i := 0; i < 5; i++ {
		c.Append(leafN(i))
	}
	head, _ := c.Flush()
	c.Close()

	s, _ := newSigner(t)
	st := s.Sign(head, 1, 1_753_700_000)
	link, leaves, idx, err := FindLeaf(dir, LeafQuery{Type: EventRemember, Scope: "user:nikolay", Subject: leafN(2).Subject})
	if err != nil {
		t.Fatal(err)
	}
	good, err := BuildProof(link, leaves, idx, st)
	if err != nil {
		t.Fatal(err)
	}

	for _, mut := range []struct {
		name  string
		apply func(*InclusionProof)
	}{
		{"предмет листа", func(p *InclusionProof) { p.Leaf.Subject = "другой факт" }},
		{"время листа", func(p *InclusionProof) { p.Leaf.UnixNano++ }},
		{"тип листа", func(p *InclusionProof) { p.Leaf.Type = uint8(EventForget) }},
		{"скоуп листа", func(p *InclusionProof) { p.Leaf.Scope = "user:другой" }},
		{"корень", func(p *InclusionProof) { p.Root = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }},
		{"шаг пути", func(p *InclusionProof) { p.Path[0].Hash = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }},
		{"сторона соседа", func(p *InclusionProof) { p.Path[0].Left = !p.Path[0].Left }},
		{"подпись заявления", func(p *InclusionProof) { p.Statement.HeadSeq += 5 }},
	} {
		edited := good
		edited.Path = append([]proofStepJSON(nil), good.Path...)
		mut.apply(&edited)
		if err := edited.Verify(s.PublicKey()); err == nil {
			t.Errorf("правка «%s» не обнаружена", mut.name)
		}
	}
}

// TestProof_RejectsLinkNewerThanStatement — доказательство про звено, которого
// подписанная голова ещё не покрывала, ничего не доказывает: заявление о
// прошлом не удостоверяет будущее.
func TestProof_RejectsLinkNewerThanStatement(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	first, _ := c.Flush()
	c.Append(leafN(1))
	second, _ := c.Flush()
	c.Close()

	s, _ := newSigner(t)
	stale := s.Sign(first, 1, 1_753_700_000) // заявление на момент ПЕРВОГО звена

	link, leaves, idx, err := FindLeaf(dir, LeafQuery{Type: EventRemember, Scope: "user:nikolay", Subject: leafN(1).Subject})
	if err != nil {
		t.Fatal(err)
	}
	if link.Seq != second.Seq {
		t.Fatalf("найдено звено %d, ожидалось %d", link.Seq, second.Seq)
	}
	proof, err := BuildProof(link, leaves, idx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.Verify(s.PublicKey()); err == nil {
		t.Fatal("доказательство про звено новее подписанной головы принято")
	}
}

func TestFindLeaf_MissReportsWindow(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir)
	c.Append(leafN(0))
	c.Flush()
	c.Close()

	if _, _, _, err := FindLeaf(dir, LeafQuery{Type: EventShred, Scope: "нет такого"}); err == nil {
		t.Fatal("несуществующее событие найдено")
	}
}

func TestSigner_RejectsCorruptKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SignKeyFileName), []byte("не ключ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(dir); err == nil {
		t.Fatal("битый файл ключа принят — инстанс молча сменил бы личность")
	}
}

func TestVerifyStatement_RejectsRandomKeyOfRightSize(t *testing.T) {
	s, _ := newSigner(t)
	st := s.Sign(Head{Seq: 1}, 1, 1)
	other := make([]byte, ed25519.PublicKeySize)
	rand.Read(other)
	if err := VerifyStatement(st, ed25519.PublicKey(other)); err == nil {
		t.Fatal("заявление прошло проверку случайным ключом")
	}
}

func containsSub(hay, needle string) bool {
	return stringContains(hay, needle)
}
