package keyring

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newRing(t *testing.T) (*Keyring, string) {
	t.Helper()
	dir := t.TempDir()
	k, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { k.Close() })
	return k, dir
}

func TestSealUnsealRoundTrip(t *testing.T) {
	k, _ := newRing(t)
	if _, err := k.EnsureScope("user:dana"); err != nil {
		t.Fatal(err)
	}
	want := []byte("Project Aurora design review approved on July 3.")
	env, err := k.Seal("user:dana", want)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(env, want) {
		t.Fatal("конверт содержит открытый текст")
	}
	got, err := k.Unseal(env)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip: got %q want %q", got, want)
	}
}

func TestEnsureScopeIdempotent(t *testing.T) {
	k, _ := newRing(t)
	a, err := k.EnsureScope("s")
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.EnsureScope("s")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("повторный EnsureScope выдал другой ключ — прежние конверты стали бы нечитаемы")
	}
}

func TestSealUnknownScopeIsDestroyedNotPanic(t *testing.T) {
	k, _ := newRing(t)
	if _, err := k.Seal("never-created", []byte("x")); !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("want ErrKeyDestroyed, got %v", err)
	}
}

// ⭐Главный тест этого пакета: после Destroy ключа НЕТ НА ДИСКЕ, а не просто
// помечен уничтоженным. Если бы кейринг был append-only (как всё остальное в
// проекте), запись «уничтожен» соседствовала бы с записью, содержащей сам
// ключ, и криптостирание было бы декоративным.
func TestDestroyRemovesKeyBytesFromDisk(t *testing.T) {
	k, dir := newRing(t)
	if _, err := k.EnsureScope("user:dana"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Вырезаем сами байты ключа из файла по формату:
	// magic | u32 len | scope | kekID(16) | key(32) | crc(4)
	off := len(fileMagic) + 4 + len("user:dana") + kekIDSize
	keyBytes := append([]byte(nil), before[off:off+kekSize]...)
	if len(keyBytes) != kekSize {
		t.Fatalf("не удалось вырезать ключ: %d байт", len(keyBytes))
	}
	if !bytes.Contains(before, keyBytes) {
		t.Fatal("предпосылка теста неверна: ключа нет в файле до Destroy")
	}

	if _, ok, err := k.Destroy("user:dana"); err != nil || !ok {
		t.Fatalf("destroy: ok=%v err=%v", ok, err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(after, keyBytes) {
		t.Fatal("после Destroy байты ключа всё ещё лежат в файле кейринга")
	}
	// И временный файл не оставлен рядом с ключом внутри.
	if tmp, err := os.ReadFile(path + ".tmp"); err == nil && bytes.Contains(tmp, keyBytes) {
		t.Fatal("ключ остался в .tmp-файле")
	}
}

// ⭐Второй главный: уничтожение переживает перезапуск. Именно это делает
// стирание действующим на снапшоты и отгруженные архивы — они попадут в
// процесс, у которого ключа уже нет.
func TestDestroySurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	k, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.EnsureScope("user:dana"); err != nil {
		t.Fatal(err)
	}
	env, err := k.Seal("user:dana", []byte("secret fact"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := k.Destroy("user:dana"); err != nil || !ok {
		t.Fatalf("destroy: %v %v", ok, err)
	}
	k.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.HasScope("user:dana") {
		t.Fatal("скоуп воскрес после перезапуска")
	}
	if _, err := reopened.Unseal(env); !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("конверт развернулся после перезапуска: %v", err)
	}
}

func TestDestroyIsIdempotent(t *testing.T) {
	k, _ := newRing(t)
	k.EnsureScope("s")
	if _, ok, _ := k.Destroy("s"); !ok {
		t.Fatal("первый Destroy должен вернуть ok")
	}
	if _, ok, err := k.Destroy("s"); ok || err != nil {
		t.Fatalf("повторный Destroy: ok=%v err=%v (ожидалось false, nil)", ok, err)
	}
}

// Стирание одного субъекта не должно задевать остальных — это ровно то
// свойство, отсутствие которого делает откат целиком негодным лечением.
func TestDestroyIsolatesScopes(t *testing.T) {
	k, _ := newRing(t)
	k.EnsureScope("user:dana")
	k.EnsureScope("user:erik")
	danaEnv, _ := k.Seal("user:dana", []byte("dana fact"))
	erikEnv, _ := k.Seal("user:erik", []byte("erik fact"))

	if _, ok, err := k.Destroy("user:dana"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := k.Unseal(danaEnv); !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("dana развернулась после стирания: %v", err)
	}
	got, err := k.Unseal(erikEnv)
	if err != nil {
		t.Fatalf("erik пострадал от чужого стирания: %v", err)
	}
	if string(got) != "erik fact" {
		t.Fatalf("erik испорчен: %q", got)
	}
}

// Различие между «ключ уничтожен» и «конверт повреждён» — не косметика:
// первое реплей обязан пережить и пропустить запись, второе означает битые
// данные и должно быть слышно.
func TestCorruptEnvelopeIsMalformedNotDestroyed(t *testing.T) {
	k, _ := newRing(t)
	k.EnsureScope("s")
	env, _ := k.Seal("s", []byte("payload"))

	corrupt := append([]byte(nil), env...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := k.Unseal(corrupt); !errors.Is(err, ErrMalformed) {
		t.Fatalf("порча хвоста: want ErrMalformed, got %v", err)
	}

	short := env[:len(envelopeTag)+kekIDSize+2]
	if _, err := k.Unseal(short); err == nil {
		t.Fatal("обрезанный конверт развернулся")
	}
}

// Записи, сделанные ДО кейринга, лежат открытым текстом. Путь чтения обязан
// их отличать, иначе миграция будет считать их зашифрованными, а квитанция —
// врать. Тот же класс ошибки, что ловил VMEM.COVERAGE для провенанса.
func TestIsEnvelopeRejectsLegacyPlaintext(t *testing.T) {
	for _, s := range [][]byte{
		[]byte("Project Aurora decision number 1 approved."),
		[]byte("EV"),
		{},
		[]byte("EV1"),
	} {
		if IsEnvelope(s) {
			t.Fatalf("легаси-текст %q принят за конверт", s)
		}
	}
	k, _ := newRing(t)
	k.EnsureScope("s")
	env, _ := k.Seal("s", []byte("x"))
	if !IsEnvelope(env) {
		t.Fatal("настоящий конверт не опознан")
	}
}

// Идентификатор ключа читается и ПОСЛЕ уничтожения: квитанция должна уметь
// сказать «этот факт был под ключом K», не имея самого K.
func TestKEKIDReadableAfterDestroy(t *testing.T) {
	k, _ := newRing(t)
	id, _ := k.EnsureScope("s")
	env, _ := k.Seal("s", []byte("x"))
	k.Destroy("s")

	got, ok := KEKIDOf(env)
	if !ok {
		t.Fatal("kekID не прочитан из конверта")
	}
	if got != id {
		t.Fatal("kekID в конверте не совпал с выданным при создании")
	}
}

func TestPersistIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	k, _ := Open(dir)
	k.EnsureScope("b")
	k.EnsureScope("a")
	k.EnsureScope("c")
	first, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	// Повторная запись того же состава обязана дать байт-в-байт тот же файл.
	k.mu.Lock()
	err = k.persistLocked()
	k.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, FileName))
	if !bytes.Equal(first, second) {
		t.Fatal("файл кейринга недетерминирован при неизменном составе")
	}
	k.Close()
}

func TestReopenPreservesKeysAndEnvelopes(t *testing.T) {
	dir := t.TempDir()
	k, _ := Open(dir)
	k.EnsureScope("user:dana")
	env, _ := k.Seal("user:dana", []byte("durable fact"))
	k.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	got, err := again.Unseal(env)
	if err != nil {
		t.Fatalf("конверт не пережил перезапуск: %v", err)
	}
	if string(got) != "durable fact" {
		t.Fatalf("got %q", got)
	}
	if scopes := again.Scopes(); len(scopes) != 1 || scopes[0] != "user:dana" {
		t.Fatalf("скоупы после перезапуска: %v", scopes)
	}
}

// Рваный хвост после краша не должен ронять открытие: целые записи читаются,
// половинчатая отбрасывается.
func TestTornTailIsTolerated(t *testing.T) {
	dir := t.TempDir()
	k, _ := Open(dir)
	k.EnsureScope("a")
	k.Close()

	path := filepath.Join(dir, FileName)
	data, _ := os.ReadFile(path)
	torn := append(append([]byte(nil), data...), []byte{0, 0, 0, 9, 'x'}...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("рваный хвост уронил открытие: %v", err)
	}
	defer again.Close()
	if !again.HasScope("a") {
		t.Fatal("целая запись потеряна из-за рваного хвоста")
	}
}
