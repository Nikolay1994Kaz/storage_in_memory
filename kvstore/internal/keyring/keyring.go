// Package keyring — кейринг KEK'ов скоупов и конверт (envelope) поверх них.
//
// Зачем. FORGET делает факт недостижимым немедленно, но байты живут дальше в
// WAL, в снятых раньше снапшотах и в отгруженных архивах (docs/VMEM_DESIGN.md,
// «Erasure guarantee»). Догнать эти копии удалением нельзя в принципе: шиппер
// возит поколения и о содержимом не знает, а снапшот уже снят. Единственный
// способ — чтобы копии с самого начала были шифротекстом, а стирание сводилось
// к уничтожению ключа. Шифровать ПОСЛЕ того, как понадобилось стереть, поздно:
// копии сделаны раньше.
//
// Три решения приняты в docs/VMEM_DESIGN.md по измеренным числам, здесь только
// их следствия для кода:
//
//  1. Граница шифротекста = граница персистентности (WAL/снапшот/архив), а не
//     путь чтения. В памяти движок работает с открытым текстом и не платит
//     ничего; платят запись и реплей (~1.9 мкс/факт).
//  2. KEK — на скоуп, в кейринге; DEK — на каждую запись, обёрнут KEK'ом и
//     едет ВМЕСТЕ с данными. Без KEK обёрнутый DEK бесполезен, поэтому архив
//     можно возить как есть, а секрет в него не попадает.
//  3. Криптостирание получается на гранулярности скоупа. Точечный FORGET
//     остаётся с прежней гарантией, и квитанция не утверждает иного.
//
// ⚠Почему кейринг НЕ append-only, в отличие от всего остального в этом
// проекте. Журнал с записью «ключ уничтожен» сохранил бы прежнюю запись с
// самим ключом — то есть уничтожение было бы декоративным. Поэтому Destroy
// переписывает файл целиком без уничтоженного ключа (tmp → fsync → rename →
// fsync каталога). Кейринг мал (32 байта на скоуп), эта цена платится редко.
// Доказательством стирания служит отдельная запись-квитанция, а не кейринг.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	// FileName — имя файла кейринга внутри каталога данных. ⚠Он не попадает
	// в снапшот и не отгружается шиппером: в этом весь смысл разделения.
	FileName = "keyring.dat"

	fileMagic   = "KRNG1\n"
	kekSize     = 32 // AES-256
	kekIDSize   = 16
	dekSize     = 32
	envelopeTag = "EV1"
)

var (
	// ErrKeyDestroyed — KEK для этой записи уничтожен (или скоупа не было).
	// ⚠Это НОРМАЛЬНОЕ состояние после криптостирания, а не повреждение:
	// реплей WAL обязан пережить его и пропустить запись, а не падать.
	ErrKeyDestroyed = errors.New("keyring: key destroyed or unknown")
	// ErrMalformed — конверт повреждён (не то же самое, что стёртый ключ).
	ErrMalformed = errors.New("keyring: malformed envelope")
	// ErrClosed — кейринг закрыт.
	ErrClosed = errors.New("keyring: closed")
)

type kek struct {
	id   [kekIDSize]byte
	key  []byte
	aead cipher.AEAD // кэш: bench показал 336 нс + 1280 Б на каждый NewGCM
}

// Keyring — набор KEK'ов по скоупам, durable в одном файле.
type Keyring struct {
	mu     sync.RWMutex
	path   string
	byName map[string]*kek
	byID   map[[kekIDSize]byte]*kek
	closed bool
}

// Open читает кейринг из каталога данных, создавая пустой, если файла нет.
func Open(dir string) (*Keyring, error) {
	k := &Keyring{
		path:   filepath.Join(dir, FileName),
		byName: make(map[string]*kek),
		byID:   make(map[[kekIDSize]byte]*kek),
	}
	data, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keyring open: %w", err)
	}
	if err := k.decode(data); err != nil {
		return nil, err
	}
	return k, nil
}

// decode разбирает содержимое файла. Формат:
//
//	"KRNG1\n" | { u32 scopeLen | scope | kekID(16) | key(32) | u32 crc } …
//
// crc считается по всей записи без самого crc — половинчатая запись после
// краша отбрасывается вместе с хвостом (ключ без данных безвреден, обратное —
// нет, поэтому кейринг пишется и fsync-ается ДО того, как данные уедут в WAL).
func (k *Keyring) decode(data []byte) error {
	if len(data) < len(fileMagic) || string(data[:len(fileMagic)]) != fileMagic {
		return fmt.Errorf("keyring: bad magic in %s", k.path)
	}
	p := data[len(fileMagic):]
	for len(p) > 0 {
		if len(p) < 4 {
			break // рваный хвост
		}
		n := int(binary.BigEndian.Uint32(p[:4]))
		recLen := 4 + n + kekIDSize + kekSize + 4
		if n < 0 || n > 1<<20 || len(p) < recLen {
			break
		}
		rec := p[:recLen]
		want := binary.BigEndian.Uint32(rec[recLen-4:])
		if crc32.ChecksumIEEE(rec[:recLen-4]) != want {
			break
		}
		scope := string(rec[4 : 4+n])
		var id [kekIDSize]byte
		copy(id[:], rec[4+n:4+n+kekIDSize])
		key := make([]byte, kekSize)
		copy(key, rec[4+n+kekIDSize:4+n+kekIDSize+kekSize])
		e, err := newKEK(id, key)
		if err != nil {
			return err
		}
		k.byName[scope] = e
		k.byID[id] = e
		p = p[recLen:]
	}
	return nil
}

func newKEK(id [kekIDSize]byte, key []byte) (*kek, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &kek{id: id, key: key, aead: aead}, nil
}

// persistLocked атомарно переписывает файл целиком. Вызывается и при создании
// ключа, и при уничтожении: полная перезапись — единственный способ, при
// котором уничтоженный ключ действительно исчезает с диска.
func (k *Keyring) persistLocked() error {
	buf := make([]byte, 0, len(fileMagic)+len(k.byName)*(kekIDSize+kekSize+64))
	buf = append(buf, fileMagic...)
	// Детерминированный порядок: файл не должен меняться от прогона к прогону
	// при неизменном содержимом (иначе не сравнить и не воспроизвести).
	names := make([]string, 0, len(k.byName))
	for name := range k.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := k.byName[name]
		start := len(buf)
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(name)))
		buf = append(buf, n[:]...)
		buf = append(buf, name...)
		buf = append(buf, e.id[:]...)
		buf = append(buf, e.key...)
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(buf[start:]))
		buf = append(buf, c[:]...)
	}

	tmp := k.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("keyring write: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return fmt.Errorf("keyring write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("keyring fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("keyring close: %w", err)
	}
	if err := os.Rename(tmp, k.path); err != nil {
		return fmt.Errorf("keyring rename: %w", err)
	}
	// fsync каталога: без него переименование может не пережить отказ питания,
	// и после перезагрузки вернётся файл со СТЁРТЫМ ключом внутри.
	d, err := os.Open(filepath.Dir(k.path))
	if err != nil {
		return fmt.Errorf("keyring dir open: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("keyring dir fsync: %w", err)
	}
	return nil
}

// EnsureScope возвращает KEK скоупа, создавая его при первом обращении.
// Идемпотентна: повторный вызов отдаёт тот же идентификатор.
func (k *Keyring) EnsureScope(scope string) ([kekIDSize]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return [kekIDSize]byte{}, ErrClosed
	}
	if e, ok := k.byName[scope]; ok {
		return e.id, nil
	}
	var id [kekIDSize]byte
	if _, err := rand.Read(id[:]); err != nil {
		return id, err
	}
	key := make([]byte, kekSize)
	if _, err := rand.Read(key); err != nil {
		return id, err
	}
	e, err := newKEK(id, key)
	if err != nil {
		return id, err
	}
	k.byName[scope] = e
	k.byID[id] = e
	if err := k.persistLocked(); err != nil {
		delete(k.byName, scope) // не оставлять ключ, которого нет на диске
		delete(k.byID, id)
		return id, err
	}
	return id, nil
}

// HasScope сообщает, существует ли живой KEK скоупа.
func (k *Keyring) HasScope(scope string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.byName[scope]
	return ok
}

// Scopes возвращает отсортированный список скоупов с живыми ключами.
func (k *Keyring) Scopes() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.byName))
	for name := range k.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Seal упаковывает открытый текст в конверт под ключом скоупа.
//
// Формат конверта (то, что уезжает в WAL/снапшот/архив):
//
//	"EV1" | kekID(16) | wrapNonce(12) | wrappedDEK(48) | dataNonce(12) | ct+tag
//
// kekID лежит открытым намеренно: по нему реплей понимает, каким ключом
// разворачивать и — главное — что ключа больше нет (стирание, а не поломка).
func (k *Keyring) Seal(scope string, plaintext []byte) ([]byte, error) {
	k.mu.RLock()
	e, ok := k.byName[scope]
	k.mu.RUnlock()
	if !ok {
		return nil, ErrKeyDestroyed
	}

	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	dataAEAD, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	wrapNonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, err
	}
	// kekID в дополнительных данных: подменить заголовок, не сломав разбор,
	// не выйдет.
	wrapped := e.aead.Seal(nil, wrapNonce, dek, e.id[:])

	dataNonce := make([]byte, dataAEAD.NonceSize())
	if _, err := rand.Read(dataNonce); err != nil {
		return nil, err
	}
	ct := dataAEAD.Seal(nil, dataNonce, plaintext, e.id[:])

	out := make([]byte, 0, len(envelopeTag)+kekIDSize+len(wrapNonce)+len(wrapped)+len(dataNonce)+len(ct))
	out = append(out, envelopeTag...)
	out = append(out, e.id[:]...)
	out = append(out, wrapNonce...)
	out = append(out, wrapped...)
	out = append(out, dataNonce...)
	out = append(out, ct...)
	return out, nil
}

// IsEnvelope сообщает, похож ли блоб на конверт. Нужен на пути чтения, чтобы
// отличить записи, сделанные до кейринга (они лежат открытым текстом и
// криптостиранию не поддаются — см. coverage), от зашифрованных.
func IsEnvelope(b []byte) bool {
	return len(b) > len(envelopeTag)+kekIDSize && string(b[:len(envelopeTag)]) == envelopeTag
}

// Unseal разворачивает конверт. Возвращает ErrKeyDestroyed, если KEK
// уничтожен — вызывающий обязан считать это штатным исходом.
func (k *Keyring) Unseal(envelope []byte) ([]byte, error) {
	if !IsEnvelope(envelope) {
		return nil, ErrMalformed
	}
	p := envelope[len(envelopeTag):]
	var id [kekIDSize]byte
	copy(id[:], p[:kekIDSize])
	p = p[kekIDSize:]

	k.mu.RLock()
	e, ok := k.byID[id]
	k.mu.RUnlock()
	if !ok {
		return nil, ErrKeyDestroyed
	}

	wn := e.aead.NonceSize()
	wrappedLen := dekSize + e.aead.Overhead()
	if len(p) < wn+wrappedLen {
		return nil, ErrMalformed
	}
	wrapNonce, wrapped := p[:wn], p[wn:wn+wrappedLen]
	p = p[wn+wrappedLen:]

	dek, err := e.aead.Open(nil, wrapNonce, wrapped, e.id[:])
	if err != nil {
		return nil, ErrMalformed
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrMalformed
	}
	dataAEAD, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrMalformed
	}
	dn := dataAEAD.NonceSize()
	if len(p) < dn {
		return nil, ErrMalformed
	}
	out, err := dataAEAD.Open(nil, p[:dn], p[dn:], e.id[:])
	if err != nil {
		return nil, ErrMalformed
	}
	return out, nil
}

// KEKIDOf достаёт идентификатор ключа из конверта, не разворачивая его.
// Нужен coverage и квитанции: сказать «этот факт был под ключом K» можно и
// после того, как K уничтожен.
func KEKIDOf(envelope []byte) ([kekIDSize]byte, bool) {
	var id [kekIDSize]byte
	if !IsEnvelope(envelope) {
		return id, false
	}
	copy(id[:], envelope[len(envelopeTag):len(envelopeTag)+kekIDSize])
	return id, true
}

// Destroy уничтожает KEK скоупа: обнуляет его в памяти и переписывает файл
// без него. После успешного возврата ни один конверт этого скоупа развернуть
// нельзя — ни здесь, ни из снапшота, ни из отгруженного архива.
//
// Возвращает идентификатор уничтоженного ключа: именно он, а не содержимое,
// попадает в квитанцию. Идемпотентна — повторный вызов даёт ok=false.
func (k *Keyring) Destroy(scope string) (id [kekIDSize]byte, ok bool, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return id, false, ErrClosed
	}
	e, exists := k.byName[scope]
	if !exists {
		return id, false, nil
	}
	id = e.id
	delete(k.byName, scope)
	delete(k.byID, id)
	if err := k.persistLocked(); err != nil {
		// Вернуть ключ в память: на диске он ещё есть, и молча делать вид,
		// что стёрли, нельзя — это ровно та ложь, ради недопущения которой
		// всё и строится.
		k.byName[scope] = e
		k.byID[id] = e
		return id, false, err
	}
	zeroize(e.key)
	e.aead = nil
	return id, true, nil
}

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Close закрывает кейринг и обнуляет ключи в памяти.
func (k *Keyring) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, e := range k.byName {
		zeroize(e.key)
		e.aead = nil
	}
	k.byName = map[string]*kek{}
	k.byID = map[[kekIDSize]byte]*kek{}
	k.closed = true
	return nil
}
