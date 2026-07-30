// Пакет auditchain — журнал событий памяти, связанный цепью хешей.
//
// ЗАЧЕМ. WAL отвечает на вопрос «как восстановить состояние», но не на вопрос
// «что с этой памятью делали». Он компактится: старые сегменты удаляются, и
// вместе с ними исчезает след того, что факт вообще существовал и был отозван.
// Квитанция VMEM.SHRED сегодня выдаётся в ответ команде и нигде не хранится —
// предъявить её через полгода нельзя. Цепь и есть носитель, на котором такая
// расписка становится неотрицаемой: запись нельзя переписать, не сломав всё,
// что после неё.
//
// ⭐ЧТО МЫ ФИКСИРУЕМ, А КОНКУРЕНТ — НЕТ. У Hakuya (разбор 28.07 по коду)
// per-tenant hash-chain зрелый, но их `verify` после десяти записей выдал
// `checked: 0`: журнал не фиксирует СОЗДАНИЕ факта. Доказать, что факта не
// подсунули задним числом, такой журнал не может. Здесь создание — событие
// первого класса.
//
// ⭐ДВЕ ДЫРЫ, ЗАКРЫТЫЕ ЗАРАНЕЕ. Их же миграция 019 чинила два дефекта в
// собственной цепи, и оба обходятся по построению:
//
//  1. НЕОДНОЗНАЧНОСТЬ РАЗДЕЛИТЕЛЕЙ. Если поля склеивать разделителем, две
//     разные записи дают одинаковый хеш: scope="a|b", id="c" неотличим от
//     scope="a", id="b|c". Здесь каждое поле кодируется с ПРЕФИКСОМ ДЛИНЫ,
//     поэтому разбор однозначен и подмена такого рода невозможна.
//  2. ОБРЕЗКА ХВОСТА. Цепь остаётся математически валидной, если отрезать
//     последние записи: каждая ссылается на предыдущую, а на следующую — нет.
//     Поэтому у записи есть монотонный seq, а состояние головы (seq + хеш)
//     хранится ОТДЕЛЬНО от журнала: усечённый файл перестаёт сходиться с
//     головой.
//
// ⚠ГРАНИЦА, КОТОРУЮ НАДО НАЗЫВАТЬ ВСЛУХ. Локальная цепь защищает от правки
// прошлого тем, кто не может переписать всё разом. Тот, кто владеет и журналом,
// и файлом головы, может отрезать хвост и пересчитать голову — против этого
// цепь бессильна в принципе, нужен внешний свидетель (публикация хеша наружу).
// Это ограничение любого локального tamper-evidence, включая чужие; врать о
// нём нельзя, поэтому оно записано здесь, а не только в доке.
package auditchain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// EventType — вид события памяти. Значения фиксированы: они попадают в хеш, и
// смена номера сломала бы проверку всех прошлых записей.
type EventType uint8

const (
	EventRemember   EventType = 1 // факт создан — то, чего нет у конкурента
	EventForget     EventType = 2 // точечный отзыв
	EventQuarantine EventType = 3 // массовый отзыв по происхождению
	EventShred      EventType = 4 // криптостирание скоупа (носитель квитанции)
	EventBackfill   EventType = 5 // миграция провенанса легаси
	EventBatch      EventType = 6 // звено-агрегат: корень Меркла над батчем листьев
)

// ⭐ЛИСТ И ЗВЕНО — ПОЧЕМУ ЭТО РАЗНЫЕ ТИПЫ.
//
// Сначала каждое событие было записью цепи. Замер темпа это отменил: событие
// весит 171 Б, а порог 10 ГБ/год пробивается уже при 1.85 события в секунду —
// измеренный темп 942…1175/с превышает его в 500–630 раз. Цепь при этом
// НЕЛЬЗЯ компактить (компакция улики — уничтожение улики), значит рост
// монотонен навсегда. Поэтому в цепь идёт одно звено на тик, а события лежат
// листьями под корнем Меркла.
//
// Отсюда и разделение типов: у листа НЕТ Seq и PrevHash. Листья не сцеплены
// между собой — их позицию задаёт порядок в батче, а неизменность доказывает
// корень. Сорок байт на событие, которые незачем платить.

// Leaf — одно событие памяти. Хранится в файле листьев, в цепь попадает
// только через корень Меркла над батчем.
//
// Payload держит специфичное для события (например, поля квитанции SHRED) и
// входит в хеш целиком. Цепь не разбирает его содержимое: её дело — доказать,
// что запись не менялась, а не понимать её.
//
// ⚠В Payload кладётся ОТПЕЧАТОК, А НЕ СОДЕРЖАНИЕ факта. Текст факта в
// append-only журнале пережил бы SHRED — стёртое жило бы вечно в том, что
// нельзя переписать. Та же логика, по которой кейринг сделан не append-only.
type Leaf struct {
	UnixNano int64
	Type     EventType
	Scope    string
	Subject  string // id факта либо иной предмет события; может быть пустым
	Payload  []byte
}

// Record — звено цепи: одна запись на тик агрегации.
//
// Для звена-агрегата (EventBatch) Scope пуст осознанно: батч охватывает
// события РАЗНЫХ скоупов, и указать один из них означало бы соврать про
// остальные. Предмет звена лежит в Payload — корень, число листьев и индекс
// первого из них.
type Record struct {
	Seq      uint64
	PrevHash [32]byte
	UnixNano int64
	Type     EventType
	Scope    string
	Subject  string // id факта либо иной предмет события; может быть пустым
	Payload  []byte
}

// hashSize — длина хеша цепи.
const hashSize = 32

// encodeForHash кодирует запись для хеширования: каждое поле с префиксом
// длины. Именно это делает подмену «переехавшего разделителя» невозможной —
// см. дыру 1 в шапке пакета.
//
// Хешируется ровно то, что записывается на диск, и в том же виде: отдельный
// «канонический» формат для хеша и другой для файла разошлись бы молча.
func encodeForHash(r Record) []byte {
	buf := make([]byte, 0, 64+len(r.Scope)+len(r.Subject)+len(r.Payload))
	var u8 [8]byte

	binary.BigEndian.PutUint64(u8[:], r.Seq)
	buf = append(buf, u8[:]...)
	buf = append(buf, r.PrevHash[:]...)
	binary.BigEndian.PutUint64(u8[:], uint64(r.UnixNano))
	buf = append(buf, u8[:]...)
	buf = append(buf, byte(r.Type))

	buf = appendField(buf, []byte(r.Scope))
	buf = appendField(buf, []byte(r.Subject))
	buf = appendField(buf, r.Payload)
	return buf
}

// appendField — поле с префиксом длины (4 байта, big-endian).
func appendField(dst, field []byte) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(field)))
	dst = append(dst, n[:]...)
	return append(dst, field...)
}

// Hash — хеш записи, включающий хеш предыдущей. Разрыв в любом месте цепи
// делает несходящимися все последующие звенья.
func Hash(r Record) [32]byte {
	return sha256.Sum256(encodeForHash(r))
}

// Head — состояние головы цепи: сколько записей и чем заканчивается.
// Хранится отдельно от журнала — это и есть защита от обрезки хвоста.
type Head struct {
	Seq  uint64
	Hash [32]byte
}

// Link строит следующую запись поверх головы. Единственный способ получить
// корректный Seq и PrevHash — чтобы «забыть связать» было нельзя.
func Link(head Head, unixNano int64, typ EventType, scope, subject string, payload []byte) Record {
	return Record{
		Seq:      head.Seq + 1,
		PrevHash: head.Hash,
		UnixNano: unixNano,
		Type:     typ,
		Scope:    scope,
		Subject:  subject,
		Payload:  payload,
	}
}

// encodeLeafForHash кодирует лист теми же полями с префиксом длины, что и
// запись, — но без Seq и PrevHash, которых у листа нет.
//
// ⚠Первым байтом идёт domainLeaf, и это не украшение: без разделения доменов
// хеш листа и хеш внутреннего узла дерева живут в одном пространстве, и узел
// можно предъявить как лист (классическая атака на второй прообраз в дереве
// Меркла). Проверяется тестом TestMerkle_NodeCannotPassAsLeaf.
func encodeLeafForHash(l Leaf) []byte {
	buf := make([]byte, 0, 32+len(l.Scope)+len(l.Subject)+len(l.Payload))
	var u8 [8]byte

	buf = append(buf, domainLeaf)
	binary.BigEndian.PutUint64(u8[:], uint64(l.UnixNano))
	buf = append(buf, u8[:]...)
	buf = append(buf, byte(l.Type))

	buf = appendField(buf, []byte(l.Scope))
	buf = appendField(buf, []byte(l.Subject))
	buf = appendField(buf, l.Payload)
	return buf
}

// batchPayloadSize — корень (32) + число листьев (4) + индекс первого листа (8).
const batchPayloadSize = 44

// BatchPayload — предмет звена-агрегата.
//
// FirstLeaf — сквозной номер первого листа батча. Он нужен, чтобы найти листья
// ПОСЛЕ ротации: файлы листьев именуются по стартовому номеру, retention
// выбрасывает их целиком, и без этого номера уцелевшее звено указывало бы
// «куда-то в прошлое». Count вместе с ним задаёт полуинтервал.
type BatchPayload struct {
	Root      [32]byte
	Count     uint32
	FirstLeaf uint64
}

func encodeBatchPayload(p BatchPayload) []byte {
	buf := make([]byte, batchPayloadSize)
	copy(buf[0:32], p.Root[:])
	binary.BigEndian.PutUint32(buf[32:36], p.Count)
	binary.BigEndian.PutUint64(buf[36:44], p.FirstLeaf)
	return buf
}

// DecodeBatchPayload разбирает предмет звена-агрегата.
func DecodeBatchPayload(b []byte) (BatchPayload, error) {
	if len(b) != batchPayloadSize {
		return BatchPayload{}, fmt.Errorf("auditchain: предмет звена %d Б, ожидалось %d", len(b), batchPayloadSize)
	}
	var p BatchPayload
	copy(p.Root[:], b[0:32])
	p.Count = binary.BigEndian.Uint32(b[32:36])
	p.FirstLeaf = binary.BigEndian.Uint64(b[36:44])
	return p, nil
}

// LinkBatch строит звено над батчем листьев: считает корень и связывает с
// головой. Единственный способ получить корректное звено — чтобы «сложить
// корень мимо цепи» было нельзя.
//
// Пустой батч звена не даёт: писать «за эту секунду ничего не было» тридцать
// миллионов раз в год — это 3.5 ГБ пустоты и весь бюджет объёма, потраченный
// на простаивающий инстанс (на живом :6381 темп 2.2 факта в СУТКИ).
func LinkBatch(head Head, unixNano int64, firstLeaf uint64, leaves []Leaf) (Record, error) {
	if len(leaves) == 0 {
		return Record{}, fmt.Errorf("auditchain: звено над пустым батчем не строится")
	}
	payload := encodeBatchPayload(BatchPayload{
		Root:      MerkleRoot(leaves),
		Count:     uint32(len(leaves)),
		FirstLeaf: firstLeaf,
	})
	return Link(head, unixNano, EventBatch, "", "", payload), nil
}

// Verify проходит цепь целиком и возвращает её голову.
//
// Проверяется всё, что может разъехаться: связь с предыдущим хешем, монотонный
// без пропусков seq и совпадение хеша записи с пересчитанным. Опциональный
// want — сохранённая голова: если она задана, а цепь короче, значит хвост
// обрезан. Без этой сверки усечённая цепь выглядит безупречной.
func Verify(records []Record, want *Head) (Head, error) {
	var head Head
	for i, r := range records {
		if r.Seq != head.Seq+1 {
			return Head{}, fmt.Errorf("auditchain: запись %d имеет seq %d, ожидался %d — цепь с пропуском",
				i, r.Seq, head.Seq+1)
		}
		if r.PrevHash != head.Hash {
			return Head{}, fmt.Errorf("auditchain: запись %d (seq %d) не связана с предыдущей — прошлое переписано",
				i, r.Seq)
		}
		head = Head{Seq: r.Seq, Hash: Hash(r)}
	}
	if want != nil {
		// Сверка длины — ДИАГНОСТИКА, а не независимая защита: seq входит в
		// хеш, поэтому расхождение почти всегда всплывёт и на сравнении
		// хешей ниже. Проверено мутацией — снятие этой ветки цепь не
		// ослабляет. Оставлена ради внятного сообщения: «хвост обрезан»
		// говорит оператору, что делать, а «голова не совпадает» — нет.
		if want.Seq != head.Seq {
			return Head{}, fmt.Errorf("auditchain: в журнале %d записей, голова помнит %d — хвост обрезан",
				head.Seq, want.Seq)
		}
		if want.Hash != head.Hash {
			return Head{}, fmt.Errorf("auditchain: голова журнала не совпадает с сохранённой — цепь подменена")
		}
	}
	return head, nil
}
