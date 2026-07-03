package main

import "testing"

// Семантик pub/sub (VSIM.SUBSCRIBE/UNSUBSCRIBE/PUBLISH) — дешёвая валидация.
//
// Покрываем input-validation surface (арность/threshold/float) — она
// goroutine-free: все error-ветки возвращаются ДО старта writePump. Ядро
// маршрутизации (SemanticSubscribe/Publish) тестируется в пакете pubsub (77%);
// здесь — контракт диспетчера на разбор аргументов, где заводятся баги.
//
// В харнессе hub создан как NewHub(nil) (semIndex=nil) — как classic-only прод-
// конфиг: SemanticPublish/Unsubscribe nil-safe, а valid-args SUBSCRIBE отвечает
// «not configured» (тоже без горутины). Success-путь с реальным semIndex
// (стартует writePump) сознательно не трогаем — его ядро уже в pubsub.

func TestExec_VsimSubscribeValidation(t *testing.T) {
	e := newExecEnv(t)

	// Арность: нужен threshold + хотя бы одна компонента.
	e.wantErrPrefix(e.do("VSIM.SUBSCRIBE", "0.5"), "ERR usage")

	// Отрицательный и нечисловой threshold.
	e.wantErrPrefix(e.do("VSIM.SUBSCRIBE", "-1", "1", "0", "0"), "ERR invalid threshold")
	e.wantErrPrefix(e.do("VSIM.SUBSCRIBE", "abc", "1", "0", "0"), "ERR invalid threshold")

	// Битая компонента вектора.
	e.wantErrPrefix(e.do("VSIM.SUBSCRIBE", "0.5", "1", "bad", "0"), "ERR invalid float")

	// Валидные аргументы, но семантик-роутинг выключен (semIndex=nil) →
	// внятная ошибка, а НЕ старт битого writePump.
	e.wantErrPrefix(e.do("VSIM.SUBSCRIBE", "0.5", "1", "0", "0"), "ERR semantic pub/sub not configured")
}

func TestExec_VsimPublishValidation(t *testing.T) {
	e := newExecEnv(t)

	// Арность: нужно message + хотя бы одна компонента.
	e.wantErrPrefix(e.do("VSIM.PUBLISH", "msg"), "ERR usage")

	// Битая компонента вектора.
	e.wantErrPrefix(e.do("VSIM.PUBLISH", "msg", "1", "bad", "0"), "ERR invalid float")

	// Happy-path без подписчиков (semIndex=nil) → 0 доставок, без горутин.
	e.wantInt(e.do("VSIM.PUBLISH", "msg", "1", "0", "0"), 0)
}

func TestExec_VsimUnsubscribeIdempotent(t *testing.T) {
	e := newExecEnv(t)
	// UNSUBSCRIBE без активной подписки → идемпотентный OK, не ошибка/паника.
	e.wantSimple(e.do("VSIM.UNSUBSCRIBE"), "OK")
}
