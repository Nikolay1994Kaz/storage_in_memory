package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// issueCert выпускает сертификат (self-signed если parent==nil, иначе подписанный
// родителем) и возвращает DER + приватный ключ. Вспомогалка для TLS-тестов.
func issueCert(t *testing.T, cn string, isCA bool, ips []net.IP, parentDER []byte, parentKey *ecdsa.PrivateKey) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           ips,
		IsCA:                  isCA,
		BasicConstraintsValid: true,
	}
	parentTmpl := tmpl
	signKey := key
	if parentDER != nil {
		pc, err := x509.ParseCertificate(parentDER)
		if err != nil {
			t.Fatalf("parse parent: %v", err)
		}
		parentTmpl = pc
		signKey = parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentTmpl, &key.PublicKey, signKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der, key
}

func writePEM(t *testing.T, dir, name string, der []byte, key *ecdsa.PrivateKey) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, name+".crt")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if key != nil {
		keyPath = filepath.Join(dir, name+".key")
		kb, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
		if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
			t.Fatalf("write key: %v", err)
		}
	}
	return certPath, keyPath
}

func TestParseTLSVersion(t *testing.T) {
	if v, err := parseTLSVersion("1.2"); err != nil || v != tls.VersionTLS12 {
		t.Fatalf(`"1.2" → %v, %v`, v, err)
	}
	if v, err := parseTLSVersion("1.3"); err != nil || v != tls.VersionTLS13 {
		t.Fatalf(`"1.3" → %v, %v`, v, err)
	}
	if _, err := parseTLSVersion("1.1"); err == nil {
		t.Fatal("ожидали ошибку на TLS 1.1 (снят как небезопасный)")
	}
}

// TestBuildServerTLSConfig_Wiring — MinVersion проставлен; mTLS включается только
// при заданном client-CA.
func TestBuildServerTLSConfig_Wiring(t *testing.T) {
	dir := t.TempDir()
	caDER, caKey := issueCert(t, "test-ca", true, nil, nil, nil)
	srvDER, srvKey := issueCert(t, "server", false, []net.IP{net.ParseIP("127.0.0.1")}, caDER, caKey)
	crt, key := writePEM(t, dir, "server", srvDER, srvKey)
	caPath, _ := writePEM(t, dir, "ca", caDER, nil)

	// Без client-CA: mTLS выключен, MinVersion=1.2.
	cfg, err := buildServerTLSConfig(crt, key, "1.2", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion=%x, want TLS1.2", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("без CA ClientAuth=%v, ожидали NoClientCert", cfg.ClientAuth)
	}

	// С client-CA: требуем и верифицируем клиентский серт.
	cfg2, err := buildServerTLSConfig(crt, key, "1.3", caPath)
	if err != nil {
		t.Fatalf("build mTLS: %v", err)
	}
	if cfg2.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("с CA ClientAuth=%v, ожидали RequireAndVerifyClientCert", cfg2.ClientAuth)
	}
	if cfg2.ClientCAs == nil {
		t.Fatal("ClientCAs не установлен при заданном client-CA")
	}

	// Битый client-CA → ошибка, а не тихо-без-mTLS.
	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("not a pem"), 0600)
	if _, err := buildServerTLSConfig(crt, key, "1.2", bad); err == nil {
		t.Fatal("ожидали ошибку на битом client-CA")
	}
}

// TestMTLS_Handshake — сквозной страж: с включённым mTLS клиент БЕЗ сертификата
// отвергается, а клиент с сертом, подписанным нашим CA, проходит хендшейк.
func TestMTLS_Handshake(t *testing.T) {
	dir := t.TempDir()
	caDER, caKey := issueCert(t, "ca", true, nil, nil, nil)
	srvDER, srvKey := issueCert(t, "server", false, []net.IP{net.ParseIP("127.0.0.1")}, caDER, caKey)
	cliDER, cliKey := issueCert(t, "client", false, nil, caDER, caKey)
	crt, key := writePEM(t, dir, "server", srvDER, srvKey)
	caPath, _ := writePEM(t, dir, "ca", caDER, nil)

	cfg, err := buildServerTLSConfig(crt, key, "1.2", caPath)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Триггерим хендшейк и закрываем.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			c.Close()
		}
	}()

	caPool := x509.NewCertPool()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPool.AppendCertsFromPEM(caPEM)
	addr := ln.Addr().String()

	// 1. Клиент БЕЗ клиентского сертификата → сервер обязан отклонить.
	// Пинним TLS 1.2: там отказ по отсутствию клиентского серта выявляется прямо
	// на хендшейке (Dial возвращает ошибку). В TLS 1.3 клиент считает хендшейк
	// завершённым до серверного алерта, и Dial вернул бы nil — тест был бы флейки.
	noCert := &tls.Config{
		RootCAs:    caPool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, noCert)
	if err == nil {
		conn.Close()
		t.Fatal("клиент без сертификата прошёл mTLS — сервер не требует клиентский серт")
	}

	// 2. Клиент С валидным клиентским сертификатом → хендшейк успешен.
	clientCert := tls.Certificate{Certificate: [][]byte{cliDER}, PrivateKey: cliKey}
	withCert := &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{clientCert},
		ServerName:   "127.0.0.1",
	}
	conn2, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, withCert)
	if err != nil {
		t.Fatalf("клиент с валидным сертификатом отвергнут: %v", err)
	}
	conn2.Close()
}
