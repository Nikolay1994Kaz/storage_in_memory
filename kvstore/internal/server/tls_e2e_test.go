package server

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSignedTLS выпускает самоподписанный сертификат на 127.0.0.1 и отдаёт
// серверный конфиг и пул с этим сертификатом для клиента.
func selfSignedTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kvstore-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}, pool
}

func startTLSServer(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	cfg, pool := selfSignedTLS(t)
	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("PONG")
	})
	srv.TLSConfig = cfg
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv.listener.Addr().String(), pool
}

// TestTLS_ServesCommands — сервер с TLSConfig обязан обслуживать команды.
//
// Регресс: epoll-слой доставал fd приведением conn.(*net.TCPConn), а TLS-листенер
// отдаёт *tls.Conn → panic в acceptLoop на ПЕРВОМ соединении, процесс умирал.
// TLS-режим (флаги -tls-cert/-tls-key) не работал ни дня с момента появления.
func TestTLS_ServesCommands(t *testing.T) {
	addr, pool := startTLSServer(t)

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if line != "+PONG\r\n" {
		t.Fatalf("ответ %q, ожидали +PONG", line)
	}
}

// TestTLS_PipelinedRecordsNotStuck — две команды двумя TLS-записями подряд.
//
// Ловушка, в которую попадает «минимальный фикс» epoll-слоя (fd через
// tls.Conn.NetConn()): tls.Conn вычитывает из сокета ОБЕ записи в свой буфер,
// а Read отдаёт только первую. Сокет пуст → epoll больше не просыпается → вторая
// команда висит, пока клиент не пришлёт что-то ещё. Сервер, читающий
// соединение циклом Read, этой дыры не имеет.
func TestTLS_PipelinedRecordsNotStuck(t *testing.T) {
	addr, pool := startTLSServer(t)

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Два отдельных Write = две TLS-записи, без ожидания ответа между ними.
	for i := 0; i < 2; i++ {
		if _, err := conn.Write([]byte("PING\r\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	r := bufio.NewReader(conn)
	for i := 0; i < 2; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("ответ %d из 2 не пришёл: %v", i+1, err)
		}
		if line != "+PONG\r\n" {
			t.Fatalf("ответ %d: %q, ожидали +PONG", i+1, line)
		}
	}
}
