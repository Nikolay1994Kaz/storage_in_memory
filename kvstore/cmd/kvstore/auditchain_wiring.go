package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strconv"
	"time"

	"kvstore/kvstore/internal/auditchain"
)

// Подключение цепи аудита к серверу.
//
// ЗАЧЕМ ОТДЕЛЬНЫМ ФАЙЛОМ, А НЕ В main(). Ровно по той же причине, что
// snapshotCryptoFor и walApplier: код, живущий замыканием внутри main,
// проверяется только ЗЕРКАЛОМ в тесте, а зеркало расходится с оригиналом
// молча. Здесь цена такого расхождения выше обычного — это единственный
// журнал, по которому потом отвечают «что с этой памятью делали».
//
// ⭐ЧТО КЛАДЁТСЯ В ЛИСТ: ОТПЕЧАТОК, А НЕ СОДЕРЖАНИЕ. Текст факта в
// append-only журнале пережил бы VMEM.SHRED — стёртое жило бы вечно в том, что
// нельзя переписать, и криптостирание стало бы декоративным. Поэтому в лист
// едет sha256 текста: он доказывает, что подсунули НЕ ТОТ факт, и ничего не
// рассказывает о содержании. Это ровно та дыра, которой нет закрытия у
// конкурента — у Hakuya журнал не фиксирует создание, и их verify подмену
// content не ловит в принципе.

// auditChain — носитель цепи этого процесса или nil, если журнал выключен.
// Пакетная переменная по той же причине, что sealValue и activeKeyring:
// executeCommand зовётся и из тестов, и протаскивать носитель ещё одним
// параметром через всю сигнатуру дороже, чем одна точка подмены.
var auditChain *auditchain.Carrier

// auditTickInterval — период агрегации батча.
//
// ⚠ЭТО НЕ ТИК WAL-СИНКЕРА (100 мс), И РАЗНИЦА ИЗМЕРЕНА. Цепь не компактится,
// поэтому её объём растёт вечно: при 100 мс это 54 ГБ/год — мимо порога 10
// ГБ/год в 5.4 раза, при 1 с — 3.56 ГБ/год. Растягивание периода бьёт только
// по рядовым REMEMBER; доказываемые моменты (SHRED, QUARANTINE) форсируют
// флаш до себя и остаются точными.
const auditTickInterval = time.Second

// auditChainDir — подкаталог носителя внутри dataDir.
const auditChainDir = "auditchain"

// startAuditChain поднимает носитель и фоновый тик. Возвращает функцию
// остановки: она глушит тик и закрывает носитель, сбрасывая остаток буфера.
func startAuditChain(dataDir string) (func(), error) {
	c, err := auditchain.Open(filepath.Join(dataDir, auditChainDir))
	if err != nil {
		return nil, err
	}
	auditChain = c

	// ⭐О НАЙДЕННОМ ПРИ ВОССТАНОВЛЕНИИ ГОВОРИМ ВСЛУХ И РАЗНЫМИ УРОВНЯМИ.
	// «Голова отставала» — след обычной аварии, «листья без корня» — тоже;
	// а вот оборванный хвост стоит увидеть глазами, потому что отличить его
	// от попытки правки может только человек, знающий, был ли отказ.
	rec := c.Recovery()
	slog.Info("audit chain opened", "dir", filepath.Join(dataDir, auditChainDir),
		"links", rec.Links, "head_seq", c.Head().Seq, "tick", auditTickInterval)
	if rec.HeadAdvanced > 0 || rec.LeavesWithoutRoot > 0 || rec.TornTailBytes > 0 {
		slog.Warn("audit chain recovered after an unclean stop",
			"head_lagged_links", rec.HeadAdvanced,
			"leaves_without_root", rec.LeavesWithoutRoot,
			"torn_tail_bytes", rec.TornTailBytes)
	}

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(auditTickInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if _, err := c.Flush(); err != nil {
					slog.Error("audit chain: batch not written, events stay unproved", "err", err)
				}
			}
		}
	}()

	return func() {
		close(stop)
		<-done
		if err := c.Close(); err != nil {
			slog.Error("audit chain close error", "err", err)
		}
		auditChain = nil
	}, nil
}

// auditAppend кладёт событие в буфер цепи. Вне тика на диск не ходит: цена
// синхронной записи измерена и составляет восемь бюджетов вставки.
//
// ⭐ЗОВЁТСЯ ПОСЛЕ ТОГО, КАК ОПЕРАЦИЯ УДАЛАСЬ, А НЕ ДО. Если бы лист
// добавлялся заранее, отказ операции оставлял бы в цепи запись о том, чего не
// было, — а это хуже отсутствия записи: цепь не может себе позволить
// утверждать лишнее. Обратный порядок стоит нам лишь того, что при отказе
// между операцией и листом событие останется НЕДОКАЗАННЫМ, — тот же выбор,
// что и в порядке fsync внутри тика.
func auditAppend(typ auditchain.EventType, scope, subject string, payload []byte) {
	if auditChain == nil {
		return
	}
	auditChain.Append(auditchain.Leaf{
		UnixNano: time.Now().UnixNano(),
		Type:     typ,
		Scope:    scope,
		Subject:  subject,
		Payload:  payload,
	})
}

// auditAppendSync — событие, чья доказуемость и есть продукт: оно форсирует
// флаш цепи ДО СЕБЯ, поэтому квитанция всегда покрывает всё, что было раньше,
// и окно агрегации не накрывает доказываемый момент.
func auditAppendSync(typ auditchain.EventType, scope, subject string, payload []byte) (uint64, error) {
	if auditChain == nil {
		return 0, nil
	}
	head, err := auditChain.AppendSync(auditchain.Leaf{
		UnixNano: time.Now().UnixNano(),
		Type:     typ,
		Scope:    scope,
		Subject:  subject,
		Payload:  payload,
	})
	return head.Seq, err
}

// Предметы событий. JSON, а не компактный бинарь, осознанно: эти байты
// поедут аудитору подписанным экспортом (П8), и проверяющий инструмент не
// должен ради них тащить наш декодер. Поля короткие, потому что лист
// пишется на каждое событие.
type rememberPayload struct {
	Hash       string `json:"h"`             // sha256 дословного текста, base64
	Source     string `json:"src,omitempty"` // происхождение — то, по чему идёт отзыв
	Sealed     bool   `json:"sealed"`        // ушёл ли факт на диск под конвертом
	Supersedes string `json:"sup,omitempty"`
}

type quarantinePayload struct {
	Source string `json:"src"`
	Since  int64  `json:"since,omitempty"`
	Facts  int    `json:"n"`
}

type shredPayload struct {
	KEKID string `json:"kek_id"`
	Facts int    `json:"n"`
}

type backfillPayload struct {
	Source string `json:"src"`
	Facts  int    `json:"n"`
}

func auditJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Предмет события собирается из наших же строк и чисел; ошибка здесь
		// означала бы дефект кода, а не входных данных. Молчать нельзя:
		// лист без предмета доказывает лишь «что-то произошло».
		slog.Error("audit chain: payload not encoded", "err", err)
		return nil
	}
	return b
}

// auditRemember — создание факта. ⭐Событие первого класса: журнал, который
// фиксирует только удаления, не может доказать, что факт не подсунули задним
// числом.
func auditRemember(scope, id, source string, sealed bool, supersedes string, text []byte) {
	sum := sha256.Sum256(text)
	auditAppend(auditchain.EventRemember, scope, id, auditJSON(rememberPayload{
		Hash:       base64.RawStdEncoding.EncodeToString(sum[:]),
		Source:     source,
		Sealed:     sealed,
		Supersedes: supersedes,
	}))
}

// auditForget — точечный отзыв. Батчем: в mix-прогоне их ~118/с, а
// синхронная запись стоит 2.78 мс, то есть треть всего времени ушла бы на
// одну команду.
func auditForget(scope, id string) {
	auditAppend(auditchain.EventForget, scope, id, nil)
}

// auditQuarantine — массовый отзыв по происхождению.
//
// Лист на КАЖДЫЙ отозванный факт, и лишь потом итоговый — синхронно. Сводка
// «отозвано N по источнику S» без поимённого списка доказывала бы объём, но не
// состав, а отзыв как раз и оспаривают пофактно. Все листья попадают в один
// батч: форс-флаш итогового накрывает их разом.
func auditQuarantine(scope, source string, since int64, ids []string) (uint64, error) {
	for _, id := range ids {
		auditAppend(auditchain.EventQuarantine, scope, id, nil)
	}
	return auditAppendSync(auditchain.EventQuarantine, scope, "", auditJSON(quarantinePayload{
		Source: source,
		Since:  since,
		Facts:  len(ids),
	}))
}

// auditShred — криптостирание скоупа: носитель квитанции, ради которого вся
// цепь и строилась.
func auditShred(scope string, kekID []byte, facts int) (uint64, error) {
	return auditAppendSync(auditchain.EventShred, scope, "", auditJSON(shredPayload{
		KEKID: hex.EncodeToString(kekID),
		Facts: facts,
	}))
}

// auditBackfill — миграция провенанса легаси. Батчем: это администраторская
// операция над прошлым, а не доказываемый момент.
func auditBackfill(scope, source string, facts int) {
	auditAppend(auditchain.EventBackfill, scope, "", auditJSON(backfillPayload{
		Source: source,
		Facts:  facts,
	}))
}

// auditSeqField — значение поля chain_seq в квитанции.
//
// ⚠Три разных исхода НЕ СХЛОПЫВАЮТСЯ в одно число. «off» — цепи нет, и
// квитанция ровно так же неотрицаема, как вчера, то есть никак. «unrecorded» —
// цепь есть, но записать не вышло; стирание при этом ПРОИЗОШЛО, и умолчать о
// нём было бы хуже, чем признать пробел в журнале. Номер звена — единственный
// случай, когда квитанцию можно предъявить.
func auditSeqField(seq uint64, err error) string {
	switch {
	case auditChain == nil:
		return "off"
	case err != nil:
		return "unrecorded"
	default:
		return strconv.FormatUint(seq, 10)
	}
}
