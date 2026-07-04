package ship

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"kvstore/kvstore/internal/wal"
)

// fakeS3 — in-memory S3-совместимый сервер для теста клиента: path-style
// PUT/GET/DELETE объектов + ListObjectsV2. Подпись не проверяет (это тест
// клиента, не сервера), но требует её наличия и обязательных SigV4-заголовков.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // ключ = "bucket/key"
	t       *testing.T
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") ||
		!strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") ||
		!strings.Contains(auth, "Signature=") {
		f.t.Errorf("bad Authorization header: %q", auth)
		http.Error(w, "bad auth", http.StatusForbidden)
		return
	}
	if r.Header.Get("x-amz-content-sha256") == "" || r.Header.Get("x-amz-date") == "" {
		f.t.Error("missing x-amz-content-sha256 / x-amz-date")
		http.Error(w, "bad headers", http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()

	// ListObjectsV2: GET /bucket?list-type=2&prefix=...
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		bucket := path
		prefix := r.URL.Query().Get("prefix")
		type contents struct {
			Key string `xml:"Key"`
		}
		var result struct {
			XMLName     xml.Name   `xml:"ListBucketResult"`
			IsTruncated bool       `xml:"IsTruncated"`
			Contents    []contents `xml:"Contents"`
		}
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, bucket+"/"+prefix) {
				keys = append(keys, strings.TrimPrefix(k, bucket+"/"))
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			result.Contents = append(result.Contents, contents{Key: k})
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(result)
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[path] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if data, ok := f.objects[path]; ok {
			w.Write(data)
		} else {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
		}
	case http.MethodDelete:
		delete(f.objects, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func newS3TestRemote(t *testing.T) (*s3Remote, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}, t: t}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	t.Setenv("KVSTORE_S3_ACCESS_KEY", "test-access")
	t.Setenv("KVSTORE_S3_SECRET_KEY", "test-secret")

	u, err := url.Parse(fmt.Sprintf("s3://test-bucket/kv/prod?endpoint=%s&region=us-east-1", srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := newS3Remote(u)
	if err != nil {
		t.Fatal(err)
	}
	return remote, fake
}

func TestS3Remote_RoundTrip(t *testing.T) {
	remote, fake := newS3TestRemote(t)
	ctx := context.Background()

	// Put → Get байт-в-байт, ключ ложится под prefix из URL.
	payload := []byte("wal chunk bytes \x00\x01\x02")
	if err := remote.Put(ctx, "wal/wal_0001.log/0000000000000000.chunk", payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.objects["test-bucket/kv/prod/wal/wal_0001.log/0000000000000000.chunk"]; !ok {
		t.Fatalf("объект не под ожидаемым ключом: %v", keysOf(fake.objects))
	}
	got, err := remote.Get(ctx, "wal/wal_0001.log/0000000000000000.chunk")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("payload roundtrip mismatch")
	}

	// List возвращает ключи относительно prefix'а URL.
	if err := remote.Put(ctx, "MANIFEST.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	keys, err := remote.List(ctx, "wal/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "wal/wal_0001.log/0000000000000000.chunk" {
		t.Fatalf("list = %v", keys)
	}

	// Get отсутствующего → ErrNotExist; Delete идемпотентен.
	if _, err := remote.Get(ctx, "nope"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if err := remote.Delete(ctx, "MANIFEST.json"); err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(ctx, "MANIFEST.json"); err != nil {
		t.Fatalf("повторный delete должен быть идемпотентен: %v", err)
	}
}

// TestS3Remote_ShipperE2E — весь цикл шиппера поверх S3-бэкенда.
func TestS3Remote_ShipperE2E(t *testing.T) {
	remote, _ := newS3TestRemote(t)
	dataDir := t.TempDir()
	s := New(remote, dataDir, Options{RetainManifests: 2})
	ctx := context.Background()

	writeWAL(t, dataDir+"/wal_0001.log",
		wal.Entry{Op: wal.OpSet, Key: "a", Value: []byte("1")},
		wal.Entry{Op: wal.OpSet, Key: "b", Value: []byte("2")},
	).Close()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	got := restoreAndReplay(t, remote, t.TempDir())
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("state = %v", got)
	}
}

func TestS3Remote_CredentialsRequired(t *testing.T) {
	t.Setenv("KVSTORE_S3_ACCESS_KEY", "")
	t.Setenv("KVSTORE_S3_SECRET_KEY", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	u, _ := url.Parse("s3://bucket/prefix")
	if _, err := newS3Remote(u); err == nil {
		t.Fatal("без кредов newS3Remote должен вернуть ошибку")
	}
}

func TestOpenRemote_Schemes(t *testing.T) {
	// file://host/path (двойной слэш = host-часть) — ошибка формата: путь
	// обязан быть абсолютным (file:///abs/path).
	if _, err := OpenRemote("file://relative/path"); err == nil {
		t.Fatal("file:// с host-частью должен отклоняться")
	}
	if _, err := OpenRemote("file://" + t.TempDir()); err != nil {
		t.Fatalf("file:///abs: %v", err)
	}
	if _, err := OpenRemote("ftp://x"); err == nil {
		t.Fatal("неизвестная схема должна отклоняться")
	}
}

func keysOf(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
