package vector

// Токенайзер BM25 (шаг 2 спринта, docs/BM25_HYBRID_DESIGN.md).
//
// Контракт запинен оракулом (scripts/bm25_oracle.py): выход обязан бит-в-бит
// совпадать с Python-эталоном — сверяется TestBM25TokenizerParity по golden.
// Обе стороны (вставка и запрос) зовут одну и ту же функцию: терм в постингах
// и терм запроса должны получаться одним ножом, иначе совпадений не будет.

import (
	"strings"
	"unicode"

	"github.com/kljensen/snowball/english"
	"github.com/kljensen/snowball/russian"
)

// Казахские буквы, которых нет в русском алфавите: детектор «не стеммить».
// Казахское слово без этих букв неотличимо от русского и уйдёт в snowball-ru —
// задокументированная яма v1 (см. дизайн, q06 в оракуле).
const bm25KazakhLetters = "әғқңөұүһі"

// bm25Tokenize: текст → термы. Нарезка ранами букв/цифр + lowercase,
// затем по-токенная нормализация (см. bm25NormalizeToken).
// tf не считает — свёртка в частоты остаётся вызывающему.
func bm25Tokenize(text string) []string {
	var terms []string
	var tok []rune
	flush := func() {
		if len(tok) > 0 {
			terms = append(terms, bm25NormalizeToken(string(tok)))
			tok = tok[:0]
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			tok = append(tok, unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return terms
}

// bm25NormalizeToken — маршрутизация по алфавиту токена. Порядок проверок
// важен: сплошь-кириллическое казахское слово обязано перехватиться ДО
// русского стеммера.
func bm25NormalizeToken(tok string) string {
	hasCyr, hasLat := false, false
	for _, r := range tok {
		if strings.ContainsRune(bm25KazakhLetters, r) {
			return tok // казахский: без стемминга в v1
		}
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			hasCyr = true
		case 'a' <= r && r <= 'z':
			hasLat = true
		}
	}
	// stemStopWords=true: с false kljensen возвращает стоп-слова нетронутыми
	// («once» вместо «onc»), а Python-эталон стеммит всё подряд — стоп-слова
	// мы не удаляем by design, их вес гасит IDF.
	switch {
	case hasCyr:
		// Snowball-эталон нормализует ё->е до стемминга (пьёт->пьет);
		// kljensen этого шага не делает — выравниваем сами.
		return russian.Stem(strings.ReplaceAll(tok, "ё", "е"), true)
	case hasLat:
		return english.Stem(tok, true)
	default:
		return tok // числа и прочее — как есть
	}
}
