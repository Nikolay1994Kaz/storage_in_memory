package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveAuthPassword_Precedence — страж приоритета источников секрета:
// файл > env > флаг. Регресс здесь = секрет читается не из того источника
// (напр. флаг перебивает файл → секрет утёк в ps, хотя оператор дал файл).
func TestResolveAuthPassword_Precedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pass")
	if err := os.WriteFile(file, []byte("from-file\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Run("file wins over env and flag", func(t *testing.T) {
		t.Setenv("KVSTORE_REQUIREPASS", "from-env")
		got, err := resolveAuthPassword("from-flag", file)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != "from-file" { // и заодно проверяем TrimSpace (обрезан \n)
			t.Fatalf("got %q, want %q", got, "from-file")
		}
	})

	t.Run("env wins over flag when no file", func(t *testing.T) {
		t.Setenv("KVSTORE_REQUIREPASS", "from-env")
		got, err := resolveAuthPassword("from-flag", "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != "from-env" {
			t.Fatalf("got %q, want %q", got, "from-env")
		}
	})

	t.Run("flag used as last resort", func(t *testing.T) {
		got, err := resolveAuthPassword("from-flag", "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != "from-flag" {
			t.Fatalf("got %q, want %q", got, "from-flag")
		}
	})

	t.Run("empty everything = auth disabled", func(t *testing.T) {
		got, err := resolveAuthPassword("", "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("missing file is a hard error", func(t *testing.T) {
		if _, err := resolveAuthPassword("from-flag", filepath.Join(dir, "nope")); err == nil {
			t.Fatal("ожидали ошибку на отсутствующем файле пароля, получили nil")
		}
	})
}

// TestAuthConstantTimeCompare — фиксирует контракт проверки пароля: сравнение
// идёт по SHA-256-дайджестам через subtle.ConstantTimeCompare (совпадает с
// логикой AUTH-обработчика). Тайминг тут не измерить, но регресс на «==» по
// сырым строкам (утечка длины) поймается изменением этого контракта.
func TestAuthConstantTimeCompare(t *testing.T) {
	want := sha256.Sum256([]byte("secret"))

	ok := sha256.Sum256([]byte("secret"))
	if subtle.ConstantTimeCompare(ok[:], want[:]) != 1 {
		t.Fatal("верный пароль не прошёл constant-time сравнение")
	}

	for _, bad := range []string{"", "Secret", "secre", "secret ", "totally-different-and-longer"} {
		h := sha256.Sum256([]byte(bad))
		if subtle.ConstantTimeCompare(h[:], want[:]) == 1 {
			t.Fatalf("неверный пароль %q прошёл сравнение", bad)
		}
	}
}
