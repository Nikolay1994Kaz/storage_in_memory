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
// Четыре исхода, и они не равнозначны:
//
//	recorded    — факт в памяти, создание записано. Норма.
//	unrecorded  — факт в памяти, записи о создании НЕТ. Либо он старше цепи
//	              (её включили позже), либо попал в память мимо команд.
//	resurrected — журнал говорит «отозван», а факт в памяти ЕСТЬ. ⚠Самый
//	              тяжёлый случай: отзыв не сработал или состояние откатили.
//	missing     — журнал говорит «жив», а в памяти НЕТ и срок жизни не истёк.
//
// ⚠И пятый, отдельно: expired — факта нет, но у него был срок, и он прошёл.
// TTL-жнец работает внутри движка и в цепь не пишет, поэтому без явного
// разделения все истёкшие факты попали бы в missing и сверка кричала бы о
// порче на штатной работе.

// reconcileReport — расхождения по одному скоупу.
type reconcileReport struct {
	Scope       string
	InMemory    int
	Recorded    int
	Unrecorded  int
	Resurrected int
	Missing     int
	Expired     int
}

// auditReconcile сверяет живое состояние с доступной частью журнала.
//
// scopeEq пуст — все скоупы. now нужен, чтобы отличить «истёк» от «пропал»;
// передаётся, а не берётся из часов, по общему правилу проекта.
func auditReconcile(lvs *vector.LeveledVectorStore, dir, scopeEq string, now int64) ([]reconcileReport, auditchain.Coverage, error) {
	// 1. Живое состояние: id → scope.
	memory := lvs.FactScopes()

	// 2. Проигрыш журнала от старых событий к новым. Порядок обязателен:
	// факт можно создать, отозвать и создать заново, и значение имеет
	// ПОСЛЕДНЕЕ событие, а не факт наличия какого-либо.
	type journalFact struct {
		scope     string
		alive     bool
		expiresAt int64
	}
	journal := make(map[string]*journalFact)

	cov, err := auditchain.ForEachLeaf(dir, func(l auditchain.Leaf) {
		switch l.Type {
		case auditchain.EventRemember:
			var p rememberPayload
			_ = json.Unmarshal(l.Payload, &p) // предмет необязателен для учёта наличия
			journal[l.Subject] = &journalFact{scope: l.Scope, alive: true, expiresAt: p.ExpiresAt}
		case auditchain.EventForget, auditchain.EventQuarantine:
			if l.Subject == "" {
				return // сводка карантина: предмета нет, факты названы отдельными листьями
			}
			if f, ok := journal[l.Subject]; ok {
				f.alive = false
			} else {
				journal[l.Subject] = &journalFact{scope: l.Scope, alive: false}
			}
		case auditchain.EventShred:
			// Стирание скоупа гасит всё, что в нём было НА ЭТОТ МОМЕНТ.
			// Факты, созданные после, живы — потому и проигрываем по порядку,
			// а не собираем множества.
			for _, f := range journal {
				if f.scope == l.Scope {
					f.alive = false
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
		case f.alive:
			r.Recorded++
		default:
			r.Resurrected++
		}
	}
	for id, f := range journal {
		if !f.alive {
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
