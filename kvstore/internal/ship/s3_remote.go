package ship

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// s3Remote — минимальный S3-совместимый клиент (AWS Signature V4) на stdlib.
//
// Осознанно НЕ aws-sdk-go: проекту нужны 4 операции (PUT/GET/DELETE/LIST), а
// SDK тянет мегабайты зависимостей в бинарь, который до сих пор обходился
// stdlib+wazero+metrics. Path-style addressing — работает с MinIO/Ceph/
// LocalStack и с региональными endpoint'ами AWS.
//
// Креды и настройки:
//   - access/secret: env KVSTORE_S3_ACCESS_KEY / KVSTORE_S3_SECRET_KEY,
//     fallback AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN);
//     env, не флаги — секреты не светятся в ps/proc (та же политика, что
//     KVSTORE_REQUIREPASS);
//   - endpoint/region: query-параметры URL (s3://bucket/prefix?endpoint=...&region=...),
//     endpoint по умолчанию https://s3.<region>.amazonaws.com, region — us-east-1.
type s3Remote struct {
	client       *http.Client
	endpoint     string // https://host[:port] — без завершающего /
	bucket       string
	prefix       string
	region       string
	accessKey    string
	secretKey    string
	sessionToken string
}

func newS3Remote(u *url.URL) (*s3Remote, error) {
	bucket := u.Host
	if bucket == "" {
		return nil, fmt.Errorf("ship: s3 URL must be s3://bucket[/prefix], got %q", u.String())
	}
	q := u.Query()
	region := q.Get("region")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	endpoint := strings.TrimSuffix(q.Get("endpoint"), "/")
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", region)
	}

	access := os.Getenv("KVSTORE_S3_ACCESS_KEY")
	secret := os.Getenv("KVSTORE_S3_SECRET_KEY")
	if access == "" {
		access = os.Getenv("AWS_ACCESS_KEY_ID")
		secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if access == "" || secret == "" {
		return nil, fmt.Errorf("ship: s3 credentials not set " +
			"(KVSTORE_S3_ACCESS_KEY/KVSTORE_S3_SECRET_KEY or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)")
	}

	return &s3Remote{
		client:       &http.Client{Timeout: 60 * time.Second},
		endpoint:     endpoint,
		bucket:       bucket,
		prefix:       strings.Trim(u.Path, "/"),
		region:       region,
		accessKey:    access,
		secretKey:    secret,
		sessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}, nil
}

// objectKey — полный ключ в бакете (prefix из URL + относительный ключ).
func (s *s3Remote) objectKey(key string) string {
	return joinKey(s.prefix, key)
}

func (s *s3Remote) Put(ctx context.Context, key string, data []byte) error {
	resp, err := s.do(ctx, http.MethodPut, s.objectKey(key), nil, data)
	if err != nil {
		return fmt.Errorf("ship: s3 put %s: %w", key, err)
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ship: s3 put %s: %s", key, httpError(resp))
	}
	return nil
}

func (s *s3Remote) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, s.objectKey(key), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("ship: s3 get %s: %w", key, err)
	}
	defer drainClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ship: s3 get %s: %s", key, httpError(resp))
	}
	return readAll(resp.Body, key)
}

func (s *s3Remote) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, s.objectKey(key), nil, nil)
	if err != nil {
		return fmt.Errorf("ship: s3 delete %s: %w", key, err)
	}
	defer drainClose(resp)
	// 204 — удалён; 404 — уже нет (идемпотентность ретеншна).
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("ship: s3 delete %s: %s", key, httpError(resp))
	}
	return nil
}

// listBucketResult — нужное подмножество ответа ListObjectsV2.
type listBucketResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

func (s *s3Remote) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	fullPrefix := s.objectKey(prefix)
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", fullPrefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, fmt.Errorf("ship: s3 list %s: %w", prefix, err)
		}
		body, rerr := io.ReadAll(resp.Body)
		drainClose(resp)
		if rerr != nil {
			return nil, fmt.Errorf("ship: s3 list %s: %w", prefix, rerr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ship: s3 list %s: HTTP %d: %s", prefix, resp.StatusCode, truncate(body))
		}
		var result listBucketResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("ship: s3 list %s: bad XML: %w", prefix, err)
		}
		for _, c := range result.Contents {
			// Возвращаем ключи относительно prefix'а URL — как file-бэкенд.
			keys = append(keys, strings.TrimPrefix(strings.TrimPrefix(c.Key, s.prefix), "/"))
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			return keys, nil
		}
		token = result.NextContinuationToken
	}
}

// do собирает, подписывает (SigV4) и выполняет запрос.
// objKey пуст для bucket-level операций (LIST).
func (s *s3Remote) do(ctx context.Context, method, objKey string, query url.Values, body []byte) (*http.Response, error) {
	canonicalPath := "/" + s.bucket
	if objKey != "" {
		canonicalPath += "/" + uriEncodePath(objKey)
	}
	rawQuery := canonicalQuery(query)

	reqURL := s.endpoint + canonicalPath
	if rawQuery != "" {
		reqURL += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	payloadHash := sha256hex(body)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateScope := now.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if s.sessionToken != "" {
		req.Header.Set("x-amz-security-token", s.sessionToken)
	}

	// Канонические заголовки: host + все x-amz-* (lowercase, отсортированы).
	type hdr struct{ k, v string }
	headers := []hdr{{"host", req.URL.Host}}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			headers = append(headers, hdr{lk, strings.TrimSpace(vs[0])})
		}
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].k < headers[j].k })

	var canonHeaders, signedHeaders strings.Builder
	for i, h := range headers {
		canonHeaders.WriteString(h.k + ":" + h.v + "\n")
		if i > 0 {
			signedHeaders.WriteByte(';')
		}
		signedHeaders.WriteString(h.k)
	}

	canonicalRequest := strings.Join([]string{
		method,
		canonicalPath,
		rawQuery,
		canonHeaders.String(),
		signedHeaders.String(),
		payloadHash,
	}, "\n")

	scope := dateScope + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256hex([]byte(canonicalRequest)),
	}, "\n")

	// Цепочка ключей подписи: kSecret → kDate → kRegion → kService → kSigning.
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), dateScope)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders.String(), signature))

	return s.client.Do(req)
}

// uriEncodePath кодирует ключ объекта по правилам SigV4: RFC 3986 для каждого
// сегмента, '/' сохраняется. url.PathEscape не подходит — у AWS свой набор
// unreserved-символов (в частности, '+' и '=' должны кодироваться).
func uriEncodePath(p string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~/"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery — query string в каноническом виде SigV4 (сортировка по ключу,
// RFC 3986-кодирование, '+' как %20). Совпадает с тем, что уходит в URL.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		for _, v := range q[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(uriEncodeComponent(k) + "=" + uriEncodeComponent(v))
		}
	}
	return b.String()
}

func uriEncodeComponent(s string) string {
	return strings.ReplaceAll(uriEncodePath(s), "/", "%2F")
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// drainClose дочитывает и закрывает body — обязательное условие переиспользования
// keep-alive соединения http.Client.
func drainClose(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func httpError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(body))
}

func truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
