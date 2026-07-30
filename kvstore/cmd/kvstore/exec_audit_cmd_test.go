package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"kvstore/kvstore/internal/auditchain"
	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/vector"
)

// leveledOf — векторное хранилище среды как *LeveledVectorStore.
func leveledOf(t *testing.T, e *execEnv) *vector.LeveledVectorStore {
	t.Helper()
	lvs, ok := e.vec.(*vector.LeveledVectorStore)
	if !ok {
		t.Fatal("векторный стор среды не LeveledVectorStore")
	}
	return lvs
}

// enableAuditChainAt поднимает носитель в dataDir среды — так же, как это
// делает main, чтобы команды нашли его по auditChainPath().
func enableAuditChainAt(t *testing.T, e *execEnv) {
	t.Helper()
	prevDir := dataDir
	dataDir = e.dir
	c, err := auditchain.Open(auditChainPath())
	if err != nil {
		t.Fatalf("auditchain.Open: %v", err)
	}
	signer, err := auditchain.LoadOrCreateSigner(auditChainPath())
	if err != nil {
		t.Fatalf("LoadOrCreateSigner: %v", err)
	}
	prevChain, prevSigner := auditChain, auditSigner
	auditChain, auditSigner = c, signer
	t.Cleanup(func() {
		auditChain, auditSigner = prevChain, prevSigner
		dataDir = prevDir
		c.Close()
	})
}

func fieldsOf(t *testing.T, v protocol.Value) map[string]string {
	t.Helper()
	out := map[string]string{}
	for i := 0; i+1 < len(v.Array); i += 2 {
		out[v.Array[i].Str] = v.Array[i+1].Str
	}
	return out
}

func TestAudit_VerifyReportsHead(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	for i := 0; i < 3; i++ {
		e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")
		if _, err := auditChain.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	v := e.do("VMEM.AUDIT", "VERIFY")
	if v.Typ != '*' {
		t.Fatalf("VERIFY: %v", v.Str)
	}
	f := fieldsOf(t, v)
	if f["status"] != "ok" || f["head_seq"] != "3" {
		t.Fatalf("VERIFY вернула %+v, ожидались status=ok head_seq=3", f)
	}
	if f["links_checked"] != "3" {
		t.Fatalf("проверено %q звеньев, ожидалось 3", f["links_checked"])
	}
}

// ⭐TestAudit_VerifyCatchesTamperedChain — то, ради чего команда существует.
func TestAudit_VerifyCatchesTamperedChain(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)
	for i := 0; i < 3; i++ {
		e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")
		auditChain.Flush()
	}

	// ПАРНЫЙ КОНТРОЛЬ: до правки сверка проходит.
	if v := e.do("VMEM.AUDIT", "VERIFY"); v.Typ == '-' {
		t.Fatalf("целая цепь отвергнута: %v", v.Str)
	}

	path := auditChainPath() + "/chain.log"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if v := e.do("VMEM.AUDIT", "VERIFY"); v.Typ != '-' {
		t.Fatal("правка цепи не обнаружена командой VERIFY")
	}
}

// ⭐TestAudit_ExportIsVerifiableWithoutSecret — суть П8: проверить заявление
// можно, ничего не получив во владение.
func TestAudit_ExportIsVerifiableWithoutSecret(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")
	auditChain.Flush()

	v := e.do("VMEM.AUDIT", "EXPORT")
	if v.Typ != '$' {
		t.Fatalf("EXPORT: %v", v.Str)
	}
	st, err := auditchain.ParseStatement([]byte(v.Str))
	if err != nil {
		t.Fatalf("заявление не разбирается: %v", err)
	}
	if err := auditchain.VerifyStatement(st, auditSigner.PublicKey()); err != nil {
		t.Fatalf("своё заявление не проходит проверку: %v", err)
	}
	if st.HeadSeq != 1 {
		t.Fatalf("в заявлении head_seq=%d, ожидалось 1", st.HeadSeq)
	}
	// ⚠Приватного ключа в документе быть не должно ни в каком виде.
	if strings.Contains(v.Str, "priv") || bytes.Contains([]byte(v.Str), auditSigner.PublicKey()) {
		t.Fatal("в заявлении утекли сырые байты ключа")
	}
}

// ⭐TestAudit_ProveShowsOwnFactOnly — доказать своё, не показывая чужого.
func TestAudit_ProveShowsOwnFactOnly(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	mine := e.do("VMEM.REMEMBER", "alice", "TEXT", "мой факт", "SOURCE", "agent-a").Str
	e.do("VMEM.REMEMBER", "alice", "TEXT", "чужой факт в том же батче", "SOURCE", "agent-b")
	auditChain.Flush()

	v := e.do("VMEM.AUDIT", "PROVE", "alice", "ID", mine)
	if v.Typ != '$' {
		t.Fatalf("PROVE: %v", v.Str)
	}
	proof, err := auditchain.ParseProof([]byte(v.Str))
	if err != nil {
		t.Fatalf("доказательство не разбирается: %v", err)
	}
	if err := proof.Verify(auditSigner.PublicKey()); err != nil {
		t.Fatalf("своё доказательство не проходит проверку: %v", err)
	}
	if proof.Leaf.Subject != mine {
		t.Fatalf("доказательство про %q, а не про %q", proof.Leaf.Subject, mine)
	}
	if strings.Contains(v.Str, "чужой факт") {
		t.Fatal("в доказательстве виден текст соседнего факта")
	}
}

func TestAudit_ProveMissReportsHonestly(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")
	auditChain.Flush()

	if v := e.do("VMEM.AUDIT", "PROVE", "alice", "ID", "нет-такого-id"); v.Typ != '-' {
		t.Fatal("доказательство несуществующего события выдано")
	}
}

// ⭐TestAudit_ReconcileFindsResurrectedFact — П9: журнал говорит «отозван», а
// факт в памяти есть. Самое тяжёлое расхождение, и поймать его может только
// сверка двух независимых источников.
func TestAudit_ReconcileFindsResurrectedFact(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	id := e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a").Str
	auditChain.Flush()

	// ПАРНЫЙ КОНТРОЛЬ: пока всё согласовано, расхождений нет.
	v := e.do("VMEM.AUDIT", "RECONCILE", "alice")
	rep := fieldsOf(t, v.Array[1].Array[0])
	if rep["recorded"] != "1" || rep["resurrected"] != "0" || rep["unrecorded"] != "0" {
		t.Fatalf("на согласованном состоянии сверка нашла расхождения: %+v", rep)
	}

	// Отзыв записан в цепь, но из памяти факт НЕ убран — модель того, что
	// увидит сверка при несработавшем отзыве.
	auditForget("alice", id)
	if _, err := auditChain.Flush(); err != nil {
		t.Fatal(err)
	}

	v = e.do("VMEM.AUDIT", "RECONCILE", "alice")
	rep = fieldsOf(t, v.Array[1].Array[0])
	if rep["resurrected"] != "1" {
		t.Fatalf("воскресший факт не найден: %+v", rep)
	}
}

// TestAudit_ReconcileFindsUnrecordedFact — факт в памяти, записи о создании
// нет: либо он старше цепи, либо попал в память мимо команд.
func TestAudit_ReconcileFindsUnrecordedFact(t *testing.T) {
	e := newExecEnv(t)
	// Факт создаётся ДО поднятия цепи — как при включении флага на живом стенде.
	e.do("VMEM.REMEMBER", "alice", "TEXT", "древний факт", "SOURCE", "agent-a")
	enableAuditChainAt(t, e)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "новый факт", "SOURCE", "agent-a")
	auditChain.Flush()

	v := e.do("VMEM.AUDIT", "RECONCILE", "alice")
	rep := fieldsOf(t, v.Array[1].Array[0])
	if rep["in_memory"] != "2" || rep["recorded"] != "1" || rep["unrecorded"] != "1" {
		t.Fatalf("сверка вернула %+v, ожидались in_memory=2 recorded=1 unrecorded=1", rep)
	}
}

// ⭐TestAudit_ReconcileDoesNotBlameTTL — истёкший факт исчезает из памяти без
// события в цепи (жнец живёт внутри движка). Без отдельного разбора сверка
// объявила бы штатную работу порчей.
func TestAudit_ReconcileDoesNotBlameTTL(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	id := e.do("VMEM.REMEMBER", "alice", "TEXT", "недолгий факт", "SOURCE", "agent-a", "TTL", "1").Str
	e.do("VMEM.REMEMBER", "alice", "TEXT", "долгий факт", "SOURCE", "agent-a")
	auditChain.Flush()

	// Убираем истёкший факт из памяти руками — ровно то, что сделает жнец.
	lvs := leveledOf(t, e)
	if !lvs.Forget(id) {
		t.Fatal("факт не удалён")
	}

	reports, cov, err := auditReconcile(lvs, auditChainPath(), "alice", 1<<40) // «спустя века»
	if err != nil {
		t.Fatal(err)
	}
	if cov.LeavesRead != 2 {
		t.Fatalf("прочитано %d листьев, ожидалось 2", cov.LeavesRead)
	}
	if len(reports) != 1 {
		t.Fatalf("отчётов %d", len(reports))
	}
	if reports[0].Expired != 1 || reports[0].Missing != 0 {
		t.Fatalf("истёкший факт отнесён к %+v, ожидалось expired=1 missing=0", reports[0])
	}
}

// TestAudit_ReconcileFlagsUnexplainedLoss — а вот факт БЕЗ срока, пропавший из
// памяти, объяснить нечем, и молчать о нём нельзя.
func TestAudit_ReconcileFlagsUnexplainedLoss(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	id := e.do("VMEM.REMEMBER", "alice", "TEXT", "вечный факт", "SOURCE", "agent-a").Str
	auditChain.Flush()

	lvs := leveledOf(t, e)
	lvs.Forget(id) // пропажа без записи об отзыве

	reports, _, err := auditReconcile(lvs, auditChainPath(), "alice", 1_753_700_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Missing != 1 {
		t.Fatalf("необъяснимая пропажа не отмечена: %+v", reports)
	}
}

// ⭐TestAudit_ReconcileReplaysShredInOrder — стирание гасит то, что было в
// скоупе НА ТОТ МОМЕНТ. Факт, созданный после стирания, жив, и объявить его
// воскресшим — значит поднять тревогу на штатной работе. Проверяется именно
// порядок: множествами «создано» и «удалено» этот случай не различается.
func TestAudit_ReconcileReplaysShredInOrder(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)
	enableKeyring(t, e.dir)

	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт до стирания", "SOURCE", "agent-a")
	if v := e.do("VMEM.SHRED", "alice"); v.Typ == '-' {
		t.Fatalf("SHRED: %v", v.Str)
	}
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт ПОСЛЕ стирания", "SOURCE", "agent-a")
	if _, err := auditChain.Flush(); err != nil {
		t.Fatal(err)
	}

	v := e.do("VMEM.AUDIT", "RECONCILE", "alice")
	rep := fieldsOf(t, v.Array[1].Array[0])
	if rep["in_memory"] != "1" || rep["recorded"] != "1" {
		t.Fatalf("сверка вернула %+v, ожидались in_memory=1 recorded=1", rep)
	}
	if rep["resurrected"] != "0" {
		t.Fatalf("факт, созданный ПОСЛЕ стирания, объявлен воскресшим: %+v", rep)
	}
	if rep["missing"] != "0" {
		t.Fatalf("стёртый факт объявлен пропавшим: %+v", rep)
	}
}

// TestAudit_ChainOffRejectsCommand — команда без цепи обязана сказать это, а
// не отвечать пустотой, которую прочтут как «расхождений нет».
func TestAudit_ChainOffRejectsCommand(t *testing.T) {
	e := newExecEnv(t)
	if auditChain != nil {
		t.Fatal("цепь включена без флага")
	}
	for _, sub := range []string{"VERIFY", "EXPORT", "RECONCILE"} {
		if v := e.do("VMEM.AUDIT", sub); v.Typ != '-' {
			t.Errorf("VMEM.AUDIT %s без цепи вернула не ошибку: %v", sub, v)
		}
	}
}

// ⭐TestAudit_ReconcileCountsQuarantineAsRevoked — сработавший карантин это
// НОРМА, а не воскресший факт.
//
// Дефект, который тест закрепляет: EventQuarantine лежал в одной ветке с
// EventForget и помечал факт в журнале как снятый, а состояние памяти
// собиралось через FactScopes(), где quarantined_at не виден. Но карантин по
// построению ОСТАВЛЯЕТ факт в памяти — ASOF до момента отзыва обязан его
// показывать, это улика. Итог: каждый УСПЕШНЫЙ массовый отзыв давал
// resurrected = числу отозванных, то есть самый тяжёлый класс тревоги
// срабатывал ложно ровно там, где сверку и запускают, — сразу после разбора
// инцидента. Найдено многоагентным прогоном (scripts/multiagent_sim.py):
// 15 отозванных фактов дали resurrected 15.
func TestAudit_ReconcileCountsQuarantineAsRevoked(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	e.do("VMEM.REMEMBER", "alice", "TEXT", "ложь из канала", "ID", "bad", "SOURCE", "web")
	e.do("VMEM.REMEMBER", "alice", "TEXT", "честный факт", "ID", "ok", "SOURCE", "human")
	auditChain.Flush()

	// ПАРНЫЙ КОНТРОЛЬ: до отзыва расхождений нет.
	rep := fieldsOf(t, e.do("VMEM.AUDIT", "RECONCILE", "alice").Array[1].Array[0])
	if rep["recorded"] != "2" || rep["revoked"] != "0" || rep["resurrected"] != "0" {
		t.Fatalf("на согласованном состоянии сверка нашла расхождения: %+v", rep)
	}

	if n := e.do("VMEM.QUARANTINE", "alice", "SOURCE", "web").Num; n != 1 {
		t.Fatalf("отозвано %d, ожидался 1", n)
	}
	if _, err := auditChain.Flush(); err != nil {
		t.Fatal(err)
	}

	rep = fieldsOf(t, e.do("VMEM.AUDIT", "RECONCILE", "alice").Array[1].Array[0])
	if rep["revoked"] != "1" {
		t.Errorf("revoked=%s, ожидался 1 — сработавший отзыв не опознан: %+v", rep["revoked"], rep)
	}
	if rep["resurrected"] != "0" {
		t.Errorf("resurrected=%s — успешный карантин посчитан порчей, "+
			"ложная тревога в главном сценарии сверки: %+v", rep["resurrected"], rep)
	}
	if rep["recorded"] != "1" {
		t.Errorf("recorded=%s, ожидался 1 (нетронутый факт): %+v", rep["recorded"], rep)
	}
}

// ⭐TestAudit_ReconcileFindsQuarantineThatDidNotTake — обратная сторона того же
// разделения: журнал говорит «отозван», факт в памяти есть, но метки
// quarantined_at на нём НЕТ. Изъятие не доехало, а журнал уже утверждает
// обратное. Без этой проверки исправление предыдущего теста просто выключило
// бы тревогу вместо того, чтобы её уточнить.
func TestAudit_ReconcileFindsQuarantineThatDidNotTake(t *testing.T) {
	e := newExecEnv(t)
	enableAuditChainAt(t, e)

	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "ID", "x", "SOURCE", "web")
	auditChain.Flush()

	// Событие отзыва записано в цепь, а сам факт НЕ отозван — метки нет.
	if _, err := auditQuarantine("alice", "web", 0, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auditChain.Flush(); err != nil {
		t.Fatal(err)
	}

	rep := fieldsOf(t, e.do("VMEM.AUDIT", "RECONCILE", "alice").Array[1].Array[0])
	if rep["resurrected"] != "1" {
		t.Errorf("resurrected=%s, ожидался 1 — несработавший отзыв не пойман: %+v",
			rep["resurrected"], rep)
	}
	if rep["revoked"] != "0" {
		t.Errorf("revoked=%s — факт без метки зачтён как успешно отозванный: %+v",
			rep["revoked"], rep)
	}
}
