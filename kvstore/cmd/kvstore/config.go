package main

// Разбор настроек сервера: TLS, пароль AUTH, адрес прослушивания.

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
)

// buildServerTLSConfig собирает *tls.Config для серверного listener'а с явным
// MinVersion и опциональным mTLS. Если задан clientCAPath — сервер требует
// клиентский сертификат, подписанный этим CA (сетевая идентичность поверх пароля).
func buildServerTLSConfig(certPath, keyPath, minVersion, clientCAPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("не удалось загрузить сертификат/ключ: %w", err)
	}
	minVer, err := parseTLSVersion(minVersion)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}
	if clientCAPath != "" {
		caPEM, err := os.ReadFile(clientCAPath)
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать client-CA %q: %w", clientCAPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client-CA %q: не найдено ни одного PEM-сертификата", clientCAPath)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert // mTLS
	}
	return cfg, nil
}

// parseTLSVersion переводит "1.2"/"1.3" в константу crypto/tls. Более старые
// версии не поддерживаем сознательно (TLS 1.0/1.1 сняты как небезопасные).
func parseTLSVersion(s string) (uint16, error) {
	switch s {
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("недопустимая -tls-min-version %q (ожидается 1.2 или 1.3)", s)
	}
}

// resolveAuthPassword разрешает пароль AUTH в порядке приоритета:
// файл (-requirepass-file) → env (KVSTORE_REQUIREPASS) → флаг (-requirepass).
// Файл/env предпочтительнее флага, т.к. секрет не попадает в список процессов
// (ps/history//proc/<pid>/cmdline). Пустой результат = аутентификация выключена.
func resolveAuthPassword(flagVal, fileVal string) (string, error) {
	if fileVal != "" {
		b, err := os.ReadFile(fileVal)
		if err != nil {
			return "", fmt.Errorf("не удалось прочитать файл пароля %q: %w", fileVal, err)
		}
		// Trailing newline из `echo pass > file` — частый источник «неверного
		// пароля»; обрезаем окаймляющие пробелы/переводы строк.
		return strings.TrimSpace(string(b)), nil
	}
	if env := os.Getenv("KVSTORE_REQUIREPASS"); env != "" {
		return env, nil
	}
	return flagVal, nil
}

// isLoopbackBind сообщает, ограничен ли -bind локальной машиной. Пустая строка
// означает все интерфейсы (семантика net.Listen), поэтому считается открытой.
func isLoopbackBind(bind string) bool {
	if bind == "localhost" {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}
