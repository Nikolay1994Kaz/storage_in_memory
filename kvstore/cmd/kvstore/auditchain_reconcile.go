package main

import (
	"encoding/json"
	"sort"

	"kvstore/kvstore/internal/auditchain"
	"kvstore/kvstore/vector"
)

// Сверка состояния памяти с журналом.
//
// ⭐ЗАЧЕМ ЭТО ВООБЩЕ. Цепь доказывает, что журнал не переписан. Она НИЧЕГО не
// говорит о том, соответствует ли ему память. Порча агентной памяти в жизни
// выглядит не как «кто-то отредактировал журнал», а как расхождение: факт
// отозван, а RECALL его отдаёт; факт есть, а откуда взялся — неизвестно.
// Проверять журнал журналом бессмысленно; ценность появляется ровно тогда,
// когда сходятся ДВА независимых источника — живое состояние и запись о том,
// как оно таким стало.
//
// Исходы, и они не равнозначны:
//
//	recorded    — факт в памяти, создание записано. Норма.
//	revoked     — журнал говорит «отозван карантином», факт в памяти ЕСТЬ и
//	              помечен quarantined_at. ⭐Тоже НОРМА, см. ниже.
//	unrecorded  — факт в памяти, записи о создании НЕТ. Либо он старше цепи
//	              (её включили позже), либо попал в память мимо команд.
//	resurrected — журнал говорит «снят», а факт в памяти живой. ⚠Самый
//	              тяжёлый случай: отзыв не сработал или состояние откатили.
//	missing     — журнал говорит «жив», а в памяти НЕТ и срок жизни не истёк.
//
// ⚠И отдельно: expired — факта нет, но у него был срок, и он прошёл.
// TTL-жнец работает внутри движка и в цепь не пишет, поэтому без явного
// разделения все истёкшие факты попали бы в missing и сверка кричала бы о
// порче на штатной работе.
//
// ⭐ПОЧЕМУ КАРАНТИН СЧИТАЕТСЯ ОТДЕЛЬНО ОТ УДАЛЕНИЯ. Это не косметика, а
// исправление ложной тревоги. FORGET и SHRED обязаны УБРАТЬ факт из памяти —
// если он там остался, это порча. QUARANTINE устроен наоборот: он намеренно
// ОСТАВЛЯЕТ факт (текст, вектор, прикладное время) и лишь добавляет ось
// quarantined_at, потому что запись о том, во что агент верил, — улика, и
// ASOF до момента отзыва обязан её показывать. Пока обе команды попадали в
// одну ветку, каждый УСПЕШНЫЙ массовый отзыв давал resurrected = числу
// отозванных: самый тяжёлый класс тревоги срабатывал ровно в том сценарии,
// ради которого сверка и существует — сразу после разбора инцидента.
// Различает их ровно один признак, и он физический: наличие quarantined_at
// у свежайшей версии (lvs.QuarantinedFacts). Отсюда же берётся настоящая
// проверка «отзыв не сработал»: факт отозван по журналу, лежит в памяти,
// а метки на нём НЕТ.

// reconcileReport — расхождения по одному скоупу.
type reconcileReport struct {
	Scope       string
	InMemory    int
	Recorded    int
	Revoked     int
	Unrecorded  int
	Resurrected int
	Missing     int
	Expired     int
}

// factState — последнее, что журнал говорит о факте. Три состояния вместо
// прежнего bool: «снят» распадается на «удалён» (обязан исчезнуть) и
// «отозван» (обязан остаться помеченным), и требования к памяти у них разные.
type factState uint8

const (
	factAlive factState = iota
	factForgotten
	factQuarantined
)

// auditReconcile сверяет живое состояние с доступной частью журнала.
//
// scopeEq пуст — все скоупы. now нужен, чтобы отличить «истёк» от «пропал»;
// передаётся, а не берётся из часов, по общему правилу проекта.
func auditReconcile(lvs *vector.LeveledVectorStore, dir, scopeEq string, now int64) ([]reconcileReport, auditchain.Coverage, error) {
	// 1. Живое состояние: id → scope, плюс кто из них помечен как отозванный.
	memory := lvs.FactScopes()
	quarantined := lvs.QuarantinedFacts()

	// 2. Проигрыш журнала от старых событий к новым. Порядок обязателен:
	// факт можно создать, отозвать и создать заново, и значение имеет
	// ПОСЛЕДНЕЕ событие, а не факт наличия какого-либо.
	type journalFact struct {
		scope     string
		state     factState
		expiresAt int64
	}
	journal := make(map[string]*journalFact)
	mark := func(l auditchain.Leaf, st factState) {
		if f, ok := journal[l.Subject]; ok {
			f.state = st
		} else {
			journal[l.Subject] = &journalFact{scope: l.Scope, state: st}
		}
	}

	cov, err := auditchain.ForEachLeaf(dir, func(l auditchain.Leaf) {
		switch l.Type {
		case auditchain.EventRemember:
			var p rememberPayload
			_ = json.Unmarshal(l.Payload, &p) // предмет необязателен для учёта наличия
			journal[l.Subject] = &journalFact{scope: l.Scope, state: factAlive, expiresAt: p.ExpiresAt}
		case auditchain.EventForget:
			if l.Subject == "" {
				return
			}
			mark(l, factForgotten)
		case auditchain.EventQuarantine:
			if l.Subject == "" {
				return // сводка карантина: предмета нет, факты названы отдельными листьями
			}
			mark(l, factQuarantined)
		case auditchain.EventShred:
			// Стирание скоупа гасит всё, что в нём было НА ЭТОТ МОМЕНТ.
			// Факты, созданные после, живы — потому и проигрываем по порядку,
			// а не собираем множества. Стирание — именно удаление: после него
			// факт обязан ИСЧЕЗНУТЬ, а не остаться помеченным.
			for _, f := range journal {
				if f.scope == l.Scope {
					f.state = factForgotten
				}
			}
		}
	})
	if err != nil {
		return nil, cov, err
	}

	// 3. Сведение. Скоупы берутся из ОБОИХ источников: скоуп, целиком
	// пропавший из памяти, обязан быть виден именно в этом отчёте.
	byScope := make(map[string]*reconcileReport)
	get := func(scope string) *reconcileReport {
		if scopeEq != "" && scope != scopeEq {
			return nil
		}
		r, ok := byScope[scope]
		if !ok {
			r = &reconcileReport{Scope: scope}
			byScope[scope] = r
		}
		return r
	}

	for id, scope := range memory {
		r := get(scope)
		if r == nil {
			continue
		}
		r.InMemory++
		f, known := journal[id]
		switch {
		case !known:
			r.Unrecorded++
		case f.state == factAlive:
			r.Recorded++
		case f.state == factQuarantined && quarantined[id]:
			// Отозван и помечен — так и должно выглядеть сработавшее изъятие.
			r.Revoked++
		default:
			// Удалён, но лежит в памяти — порча. Либо отозван, но метки НЕТ:
			// изъятие не доехало, а журнал уже утверждает обратное.
			r.Resurrected++
		}
	}
	for id, f := range journal {
		if f.state != factAlive {
			continue
		}
		if _, alive := memory[id]; alive {
			continue
		}
		r := get(f.scope)
		if r == nil {
			continue
		}
		if f.expiresAt > 0 && f.expiresAt <= now {
			r.Expired++
		} else {
			r.Missing++
		}
	}

	out := make([]reconcileReport, 0, len(byScope))
	for _, r := range byScope {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, cov, nil
}
