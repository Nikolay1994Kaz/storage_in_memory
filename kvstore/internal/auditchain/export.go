package auditchain

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Подписанный экспорт: то, что предъявляют ТРЕТЬЕЙ СТОРОНЕ.
//
// ⭐ПОЧЕМУ ПОДПИСЬ АСИММЕТРИЧНАЯ, А НЕ HMAC. У конкурента (Hakuya, разбор
// 28.07 по коду) журнал заверяется HMAC — то есть тем же секретом, которым он
// и создаётся. Проверить такую подпись может только держатель секрета, а он и
// есть та сторона, чьи утверждения проверяют. Аудитору остаётся либо верить на
// слово, либо получить секрет — и тогда он сам сможет подделать что угодно, а
// заявление перестанет что-либо доказывать про кого-либо. Ed25519 разрывает
// этот круг: подписывает приватный ключ, который не покидает машину, проверяет
// публичный, который можно печатать в письме.
//
// ⚠ЧЕГО ПОДПИСЬ НЕ ДОКАЗЫВАЕТ, И ЭТО НАДО ГОВОРИТЬ ПЕРВЫМ. Она удостоверяет
// «этот ключ подписал такую голову», а не «владелец не переписал журнал». Тот,
// кто владеет и цепью, и ключом, перепишет цепь и подпишет заново — против
// этого локальные средства бессильны в принципе (см. шапку chain.go).
// Асимметричность добавляет ровно две вещи, и обе реальные:
//
//   - проверка БЕЗ секрета, то есть аудитор ничего не получает во владение;
//   - ⭐привязка к ИНСТАНСУ: сторона, однажды закрепившая публичный ключ,
//     заметит подмену сервера или «нового чистого» журнала — подпись перестанет
//     сходиться с закреплённым ключом.
//
// Отсюда правило проверки: публичный ключ берётся у аудитора ИЗ ЗАКРЕПЛЁННОГО
// РАНЕЕ, а не из самого заявления. Ключ из документа проверяет лишь
// внутреннюю связность документа — и VerifyStatement это различие требует
// явно, а не оставляет на внимательность.

// StatementVersion — версия формата заявления. Входит в подписываемые байты:
// смена формата не должна оставлять старые подписи «валидными» для новых
// правил разбора.
const StatementVersion = 1

// signDomain — префикс подписываемых байт. Разделение доменов на том же
// основании, что у листа и узла в дереве: подпись над заявлением не должна
// быть предъявима как подпись над чем-то ещё.
const signDomain = "vmem-audit-statement-v1"

// SignKeyFileName — приватный ключ инстанса рядом с цепью.
const SignKeyFileName = "sign.key"

// Signer — ключ подписи этого инстанса.
type Signer struct {
	priv ed25519.PrivateKey
}

// LoadOrCreateSigner читает ключ из каталога цепи, создавая его при первом
// запуске.
//
// ⚠Права 0600 и никакого копирования: приватный ключ — единственное, что
// НЕ должно уезжать вместе с данными. Он намеренно лежит не там, где кейринг:
// у них разные роли (кейринг делает копии нечитаемыми, ключ подписи делает
// заявления проверяемыми) и разная судьба при инциденте.
func LoadOrCreateSigner(dir string) (*Signer, error) {
	path := filepath.Join(dir, SignKeyFileName)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("auditchain: ключ подписи %s повреждён: %d Б вместо %d",
				path, len(data), ed25519.PrivateKeySize)
		}
		return &Signer{priv: ed25519.PrivateKey(data)}, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return nil, err
	}
	// Каталог тоже: без fsync каталога новый файл может не пережить отказ
	// питания, и следующий старт молча выпустит ДРУГОЙ ключ — то есть все
	// прежде выданные заявления перестанут сходиться.
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return &Signer{priv: priv}, nil
}

// PublicKey — то, что публикуют и закрепляют у аудитора.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// PublicKeyString — публичный ключ в base64, пригодном для письма и конфига.
func (s *Signer) PublicKeyString() string {
	return base64.RawStdEncoding.EncodeToString(s.PublicKey())
}

// Statement — заявление о состоянии цепи на момент.
//
// ⭐ОНО ЖЕ КОНТРОЛЬНАЯ ТОЧКА. Сверка цепи с нуля стоит десятки секунд на
// годовой цепи (замер в verify_bench_test.go), поэтому аудитор хранит прошлое
// заявление и проверяет только отрезок с его HeadSeq. Собственная контрольная
// точка рядом с журналом такой роли не сыграла бы: её перепишет тот же, кто
// перепишет цепь, а хранимое СНАРУЖИ заявление — нет.
type Statement struct {
	Version   int    `json:"v"`
	PublicKey string `json:"pubkey"`
	HeadSeq   uint64 `json:"head_seq"`
	HeadHash  string `json:"head_hash"`
	Links     int    `json:"links"`
	SignedAt  int64  `json:"signed_at"`
	Signature string `json:"sig"`
}

// statementBytes — канонические подписываемые байты.
//
// Кодирование с префиксом длины и явным доменом, как в самой цепи: сторонний
// проверяющий должен уметь воспроизвести их по описанию, а не по нашему коду.
// JSON для этого не годится — порядок полей и экранирование зависят от
// библиотеки, и подпись разъехалась бы между языками.
func statementBytes(st Statement) []byte {
	buf := make([]byte, 0, 128)
	buf = appendField(buf, []byte(signDomain))
	var u8 [8]byte
	binary.BigEndian.PutUint64(u8[:], uint64(st.Version))
	buf = append(buf, u8[:]...)
	buf = appendField(buf, []byte(st.PublicKey))
	binary.BigEndian.PutUint64(u8[:], st.HeadSeq)
	buf = append(buf, u8[:]...)
	buf = appendField(buf, []byte(st.HeadHash))
	binary.BigEndian.PutUint64(u8[:], uint64(st.Links))
	buf = append(buf, u8[:]...)
	binary.BigEndian.PutUint64(u8[:], uint64(st.SignedAt))
	buf = append(buf, u8[:]...)
	return buf
}

// Sign выпускает подписанное заявление о голове.
func (s *Signer) Sign(head Head, links int, unixSec int64) Statement {
	st := Statement{
		Version:   StatementVersion,
		PublicKey: s.PublicKeyString(),
		HeadSeq:   head.Seq,
		HeadHash:  base64.RawStdEncoding.EncodeToString(head.Hash[:]),
		Links:     links,
		SignedAt:  unixSec,
	}
	st.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(s.priv, statementBytes(st)))
	return st
}

// VerifyStatement проверяет подпись заявления.
//
// ⚠pinned — публичный ключ, полученный аудитором ЗАРАНЕЕ и другим каналом.
// Передать nil можно, и тогда проверяется только внутренняя связность
// документа: подпись сойдётся с ключом из него самого. Это НЕ доказательство
// происхождения — кто угодно сгенерирует пару и подпишет любую голову.
// Параметр обязателен именно поэтому: «забыть закрепить ключ» должно требовать
// явного nil, а не быть значением по умолчанию.
func VerifyStatement(st Statement, pinned ed25519.PublicKey) error {
	if st.Version != StatementVersion {
		return fmt.Errorf("auditchain: заявление версии %d, поддерживается %d", st.Version, StatementVersion)
	}
	pub, err := base64.RawStdEncoding.DecodeString(st.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("auditchain: публичный ключ в заявлении неразбираем")
	}
	if pinned != nil && !pinned.Equal(ed25519.PublicKey(pub)) {
		return errors.New("auditchain: заявление подписано ДРУГИМ ключом, чем закреплённый — подменён инстанс или журнал")
	}
	sig, err := base64.RawStdEncoding.DecodeString(st.Signature)
	if err != nil {
		return fmt.Errorf("auditchain: подпись неразбираема")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), statementBytes(st), sig) {
		return errors.New("auditchain: подпись не сходится — заявление изменено после выпуска")
	}
	return nil
}

// InclusionProof — доказательство того, что конкретное событие лежит в цепи.
//
// ⭐Показывает ТОЛЬКО своё событие: путь Меркла сходится к корню, не раскрывая
// соседних листьев. Владелец памяти может доказать «моя запись есть и не
// менялась», не предъявляя аудитору чужих людей из того же батча.
type InclusionProof struct {
	Version int `json:"v"`
	Leaf    struct {
		UnixNano int64  `json:"t"`
		Type     uint8  `json:"type"`
		Scope    string `json:"scope"`
		Subject  string `json:"subject"`
		Payload  string `json:"payload"` // base64: это байты, а не текст
	} `json:"leaf"`
	Path      []proofStepJSON `json:"path"`
	Root      string          `json:"root"`
	LinkSeq   uint64          `json:"link_seq"`
	Statement Statement       `json:"statement"`
}

type proofStepJSON struct {
	Hash string `json:"h"`
	Left bool   `json:"left"`
}

// BuildProof собирает доказательство включения листа с индексом idx внутри
// батча звена link.
func BuildProof(link Record, leaves []Leaf, idx int, st Statement) (InclusionProof, error) {
	var out InclusionProof
	p, err := DecodeBatchPayload(link.Payload)
	if err != nil {
		return out, err
	}
	path, err := MerkleProof(leaves, idx)
	if err != nil {
		return out, err
	}
	out.Version = StatementVersion
	l := leaves[idx]
	out.Leaf.UnixNano = l.UnixNano
	out.Leaf.Type = uint8(l.Type)
	out.Leaf.Scope = l.Scope
	out.Leaf.Subject = l.Subject
	out.Leaf.Payload = base64.RawStdEncoding.EncodeToString(l.Payload)
	for _, s := range path {
		out.Path = append(out.Path, proofStepJSON{
			Hash: base64.RawStdEncoding.EncodeToString(s.Hash[:]),
			Left: s.SiblingLeft,
		})
	}
	out.Root = base64.RawStdEncoding.EncodeToString(p.Root[:])
	out.LinkSeq = link.Seq
	out.Statement = st
	return out, nil
}

// VerifyProof проверяет доказательство включения целиком: подпись заявления и
// путь Меркла от листа к корню.
//
// ⚠ЧЕГО ОН НЕ ПРОВЕРЯЕТ: что звено с этим корнем действительно является
// предком подписанной головы. Для этого нужен сам файл цепи — документ
// маленький как раз потому, что не тащит её с собой. Проверяющий делает второй
// шаг сам: берёт chain.log, находит звено LinkSeq, сверяет корень и
// прогоняет Verify до головы из заявления. Порядок назван в docs/COMMANDS.md.
func (pr InclusionProof) Verify(pinned ed25519.PublicKey) error {
	if err := VerifyStatement(pr.Statement, pinned); err != nil {
		return err
	}
	payload, err := base64.RawStdEncoding.DecodeString(pr.Leaf.Payload)
	if err != nil {
		return fmt.Errorf("auditchain: предмет листа неразбираем")
	}
	leaf := Leaf{
		UnixNano: pr.Leaf.UnixNano,
		Type:     EventType(pr.Leaf.Type),
		Scope:    pr.Leaf.Scope,
		Subject:  pr.Leaf.Subject,
		Payload:  payload,
	}
	path := make([]ProofStep, 0, len(pr.Path))
	for _, s := range pr.Path {
		h, err := base64.RawStdEncoding.DecodeString(s.Hash)
		if err != nil || len(h) != hashSize {
			return fmt.Errorf("auditchain: шаг пути неразбираем")
		}
		var step ProofStep
		copy(step.Hash[:], h)
		step.SiblingLeft = s.Left
		path = append(path, step)
	}
	rootB, err := base64.RawStdEncoding.DecodeString(pr.Root)
	if err != nil || len(rootB) != hashSize {
		return fmt.Errorf("auditchain: корень неразбираем")
	}
	var root [32]byte
	copy(root[:], rootB)
	if !VerifyProof(leaf, path, root) {
		return errors.New("auditchain: путь не сходится с корнем — событие в этом батче не лежит")
	}
	if pr.LinkSeq > pr.Statement.HeadSeq {
		return fmt.Errorf("auditchain: звено %d новее подписанной головы %d — заявление к этому доказательству не относится",
			pr.LinkSeq, pr.Statement.HeadSeq)
	}
	return nil
}

// MarshalJSON-обёртки: наружу оба документа уходят текстом.
func (st Statement) JSON() ([]byte, error)      { return json.MarshalIndent(st, "", "  ") }
func (pr InclusionProof) JSON() ([]byte, error) { return json.MarshalIndent(pr, "", "  ") }
func ParseStatement(b []byte) (Statement, error) {
	var st Statement
	err := json.Unmarshal(b, &st)
	return st, err
}
func ParseProof(b []byte) (InclusionProof, error) {
	var pr InclusionProof
	err := json.Unmarshal(b, &pr)
	return pr, err
}
