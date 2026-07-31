#!/usr/bin/env bash
# Мутационная проверка запечатанного снапшота (формат v9) по канону проекта:
# внести дефект в БОЕВОЙ код, убедиться, что набор тестов краснеет, откатить.
# Выжившая мутация = дыра в тестах, а не победа.
#
# ЗАПУСК:  bash scripts/mutation_sealed_snapshot.sh   (из корня репозитория)
#
# 🚨ЛОВУШКА, СТОИВШАЯ ПОТЕРИ РАБОТЫ. Первая версия откатывала мутацию через
# `git checkout -- "$file"` — и снесла НЕЗАКОММИЧЕННЫЕ правки в тех же файлах:
# прогонщик не отличает свою мутацию от чужой работы. Откат идёт через копию
# файла. И всё равно: КОММИТЬТЕ ДО ПРОГОНА, потому что любой инструмент,
# который пишет в рабочее дерево, рано или поздно напишет не туда.
#
# РЕЗУЛЬТАТ 31.07.2026: поймано 15, выжило 4 — все четыре ЭКВИВАЛЕНТНЫ,
# разобраны в шапке kvstore/vector/snapshot_sealed_sq.go. Два прогона нашли
# два настоящих пробела: слепой оракул (сравнивались два пути, делившие один
# код) и непроверявшуюся ось — значения атрибутов.
# мутации и возвращается после, что бы в файле ни лежало.
# Страховка: перед запуском требуем чистое дерево по мутируемым файлам.

set -u
cd "$(git rev-parse --show-toplevel)"

TESTS='TestSealed|TestKeyCoverage|TestSegmentSeals|TestLoadV8|TestV8Snapshot'
caught=0
survived=0
declare -a SURVIVORS

mutate() {
  local name="$1" file="$2" from="$3" to="$4"
  cp "$file" "$file.mutbak"

  python3 - "$file" "$from" "$to" <<'PY'
import sys
path, frm, to = sys.argv[1], sys.argv[2], sys.argv[3]
src = open(path, encoding='utf-8').read()
if src.count(frm) != 1:
    print(f"SKIP-ANCHOR:{src.count(frm)}")
    sys.exit(9)
open(path, 'w', encoding='utf-8').write(src.replace(frm, to))
PY
  local rc=$?
  if [ $rc -eq 9 ]; then
    echo "  ⚠ ЯКОРЬ НЕ УНИКАЛЕН — мутация $name не применена"
    mv "$file.mutbak" "$file"
    return
  fi

  if ! go build ./... >/dev/null 2>&1; then
    echo "  ⊘ не компилируется: $name (мутация невалидна, не считается)"
    mv "$file.mutbak" "$file"
    return
  fi

  if go test ./kvstore/vector/ -run "$TESTS" -short >/dev/null 2>&1; then
    echo "  ✘ ВЫЖИЛА: $name"
    survived=$((survived+1))
    SURVIVORS+=("$name")
  else
    echo "  ✔ поймана: $name"
    caught=$((caught+1))
  fi
  mv "$file.mutbak" "$file"
}

echo "=== МУТАЦИИ: запечатывание SQ8 и hnsw (формат v9) ==="

SQ=kvstore/vector/snapshot_sealed_sq.go
LS=kvstore/vector/leveled_store.go
KC=kvstore/vector/vmem_key_coverage.go

# ── writeCodesMasked: маскирование кодов ────────────────────────────────
mutate "M1 маска не пишет нули (пишет настоящие коды)" "$SQ" \
  'if _, err := w.Write(zeros); err != nil {' \
  '_ = zeros
		if _, err := w.Write(codes[i*dim : (i+1)*dim]); err != nil {'

mutate "M2 off-by-one в продолжении прогона" "$SQ" \
  'runStart = i + 1' \
  'runStart = i'

mutate "M3 хвост слэба не дописывается" "$SQ" \
  'if err := flush(n); err != nil {' \
  'if err := flush(runStart); err != nil {'

# ── dequantAt / restoreSealedVectorsSQ: симметрия квантования ───────────
mutate "M4 деквантование теряет sqMin" "$SQ" \
  'out[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]' \
  'out[d] = float32(fg.codes[base+d]) * fg.sqScale[d]'

mutate "M5 переквантование округляет вниз, а не к ближайшему" "$SQ" \
  'qi := int(q + 0.5)' \
  'qi := int(q)'

mutate "M6 переквантование теряет sqMin" "$SQ" \
  'q := (e.Vec[d] - fg.sqMin[d]) / fg.sqScale[d]' \
  'q := e.Vec[d] / fg.sqScale[d]'

mutate "M7 нет защиты от константной размерности" "$SQ" \
  'if fg.sqScale[d] == 0 {
				fg.codes[base+d] = 0
				continue
			}' \
  'if false {
				fg.codes[base+d] = 0
				continue
			}'

# ── frozenSQEntries: разворот сегмента ──────────────────────────────────
mutate "M8 дыры (удалённые ноды) не пропускаются" "$SQ" \
  'if key == "" {
			continue // дыра: нода удалена
		}' \
  'if key == "" && false {
			continue // дыра: нода удалена
		}'

# ── врезка в SaveBinary: SQ8 ────────────────────────────────────────────
mutate "M9 SQ8 пишется без маски" "$LS" \
  'if err := s.fg.WriteGraphToSQMasked(w, sqMask); err != nil {' \
  'if err := s.fg.WriteGraphToSQMasked(w, nil); err != nil {'

mutate "M10 SQ8: публичные слои строятся из ПОЛНОГО набора" "$LS" \
  'sqAttrs = buildSegmentAttrs(sqPublic, nil)
					sqText = buildSegmentText(sqPublic)' \
  '_ = sqPublic
					sqAttrs = buildSegmentAttrs(sqEntries, nil)
					sqText = buildSegmentText(sqEntries)'

# ── врезка в SaveBinary: hnsw ───────────────────────────────────────────
mutate "M11 hnsw пишет настоящий вектор вместо нулей" "$LS" \
  'if !sealed && len(vec) > 0 {' \
  'if len(vec) > 0 {'

mutate "M12 hnsw: в секцию уезжает пустой набор" "$LS" \
  'writeErr = writeSealedDocs(w, sealedDocs, vmemScopeOf, lvs.snapshotCrypto)' \
  '_ = sealedDocs
					writeErr = writeSealedDocs(w, nil, vmemScopeOf, lvs.snapshotCrypto)'

mutate "M16 hnsw: ничто не признаётся запечатываемым" "$LS" \
  'sealed := scopeAt != nil && scopeAt(int(id)) != ""' \
  'sealed := false && scopeAt != nil && scopeAt(int(id)) != ""'

mutate "M17 hnsw: атрибуты запечатанного пишутся открыто" "$LS" \
  'attrs := Attributes{}
					if !sealed {
						attrs = s.attrs.decodeAt(int(id))
					}' \
  'attrs := s.attrs.decodeAt(int(id))
					if false {
						attrs = s.attrs.decodeAt(int(id))
					}'

mutate "M18 hnsw: термы запечатанного пишутся открыто" "$LS" \
  'if sealed {
						entryTerms = nil
					}' \
  'if false {
						entryTerms = nil
					}'

mutate "M19 scopeColumnReader: отсутствие атрибута считается скоупом" "$SQ" \
  'if c == attrMissing || int(c) >= len(vals) {' \
  'if int(c) >= len(vals) {'

# ── гейты версии ────────────────────────────────────────────────────────
mutate "M13 гейт v9 у SQ8 не срабатывает никогда" "$LS" \
  'if version >= 9 {
					sealedDocs, dead, err := readSealedDocs(r, fg.n, lvs.snapshotCrypto)' \
  'if version >= 99 {
					sealedDocs, dead, err := readSealedDocs(r, fg.n, lvs.snapshotCrypto)'

mutate "M14 гейт v9 у hnsw не срабатывает никогда" "$LS" \
  'if version >= 9 {
					sealedDocs, dead, err := readSealedDocs(r, len(entries), lvs.snapshotCrypto)' \
  'if version >= 99 {
					sealedDocs, dead, err := readSealedDocs(r, len(entries), lvs.snapshotCrypto)'

# ── белый список покрытия ───────────────────────────────────────────────
mutate "M15 белый список стал чёрным (fail-open по умолчанию)" "$KC" \
  '	default:
		return false
	}' \
  '	default:
		return true
	}'

echo
echo "════════════════════════════════════════════"
echo "поймано: $caught   выжило: $survived"
if [ $survived -gt 0 ]; then
  echo "ВЫЖИВШИЕ (= дыры в тестах):"
  printf '  - %s\n' "${SURVIVORS[@]}"
fi
git status --short
