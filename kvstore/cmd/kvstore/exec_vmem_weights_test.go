package main

import (
	"strings"
	"testing"
)

// WEIGHTS по протоколу. Ядровые тесты (vector/vmem_fusion_weights_test.go)
// проверяют арифметику слияния; здесь — что рычаг вообще доезжает до неё через
// RESP и что негодные формы отклоняются на границе, а не «работают наполовину».
func TestVMEMRecallWeightsOverRESP(t *testing.T) {
	e := newExecEnv(t)
	const scope = "s"

	// Плечи тянут в разные стороны: лексический фаворит далёк по вектору и
	// наоборот. Без этого любой вес дал бы один и тот же порядок.
	e.wantBulk(e.do("VMEM.REMEMBER", scope, "TEXT", "aerial yoga class schedule",
		"ID", "lex", "VEC", "0", "1"), "lex")
	e.wantBulk(e.do("VMEM.REMEMBER", scope, "TEXT", "completely unrelated wording",
		"ID", "vec", "VEC", "1", "0"), "vec")

	first := func(t *testing.T, args ...string) string {
		t.Helper()
		v := e.do("VMEM.RECALL", args...)
		if v.Typ != '*' || len(v.Array) < 3 {
			t.Fatalf("ответ не массив троек: %+v", v)
		}
		return v.Array[0].Str
	}

	q := []string{scope, "2", "aerial yoga class schedule"}
	t.Run("вес гасит голос плеча", func(t *testing.T) {
		onlyText := first(t, append(append([]string{}, q...),
			"WEIGHTS", "1", "0", "VEC", "1", "0")...)
		onlyVec := first(t, append(append([]string{}, q...),
			"WEIGHTS", "0", "1", "VEC", "1", "0")...)
		if onlyText == onlyVec {
			t.Fatalf("оба веса дали %s — рычаг не доехал через RESP", onlyText)
		}
		if onlyText != "lex" || onlyVec != "vec" {
			t.Errorf("WEIGHTS 1 0 → %s (ждали lex), WEIGHTS 0 1 → %s (ждали vec)",
				onlyText, onlyVec)
		}
	})

	t.Run("негодные формы отклоняются", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
			want string
		}{
			{"оба нуля", []string{"WEIGHTS", "0", "0", "VEC", "1", "0"},
				"weights must be >= 0 and not both zero"},
			{"отрицательный", []string{"WEIGHTS", "-1", "1", "VEC", "1", "0"},
				"weights must be >= 0 and not both zero"},
			{"не число", []string{"WEIGHTS", "many", "1", "VEC", "1", "0"},
				"WEIGHTS text weight not a number"},
			{"нет второго веса", []string{"WEIGHTS", "1"},
				"WEIGHTS requires <text> <vector>"},
			// ⭐Главный из этих случаев: веса без VEC применять не к чему, и
			// команда обязана упасть, а не сделать вид, что послушалась.
			{"без VEC", []string{"WEIGHTS", "1", "0"},
				"require a query vector"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				v := e.do("VMEM.RECALL", append(append([]string{}, q...), tc.args...)...)
				if v.Typ != '-' {
					t.Fatalf("ждали ошибку, получили %+v", v)
				}
				if !strings.Contains(v.Str, tc.want) {
					t.Errorf("ошибка %q не содержит %q", v.Str, tc.want)
				}
			})
		}
	})
}
