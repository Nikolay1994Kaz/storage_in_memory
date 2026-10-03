package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// legacyKeyringFile — кейринг шифрования на диске (-encrypt-at-rest) прежних
// версий. Пока он лежит в каталоге данных, в журнале и снапшотах могут быть
// записи под его ключами.
const legacyKeyringFile = "keyring.dat"

// checkDataDirSupported отказывает каталогу, записанному прежней версией с
// шифрованием на диске. Ключей у этой версии нет, а реплей конвертов как
// обычных значений положил бы в память шифротекст вместо данных: тихая порча
// вместо понятного отказа при старте.
func checkDataDirSupported(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, legacyKeyringFile)); err == nil {
		return fmt.Errorf("data directory %s was written with -encrypt-at-rest by an older version (%s is present); "+
			"this version has no encryption at rest and cannot read it: run an older version on this directory "+
			"or start with an empty -data-dir", dir, legacyKeyringFile)
	}
	return nil
}
