package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client — HTTP-клиент к Ollama API.
//
// Зачем отдельный пакет?
// Тот же принцип, что у тебя в pubsub/ и compute/ —
// каждая подсистема живёт изолированно, общается через интерфейсы.
//
// Ollama API очень простой — всего 2 эндпоинта нам нужны:
//
//	POST /api/embed  — текст → вектор ([]float32)
//	POST /api/chat   — промпт → ответ LLM (string)
type Client struct {
	baseURL    string       // "http://localhost:11434"
	httpClient *http.Client // переиспользуемый клиент (connection pooling!)
	embedModel string       // "nomic-embed-text"
	chatModel  string       // "gemma4:e2b"
}

// NewClient создаёт клиент к Ollama.
//
// Почему http.Client создаётся ОДИН РАЗ?
// Тот же принцип, что ConnState в твоём server.go —
// переиспользование вместо создания на каждый запрос.
// http.Client внутри держит пул TCP-соединений (connection pool).
// Если создавать новый на каждый запрос — каждый раз новый TCP handshake.
func NewClient(baseURL, embedModel, chatModel string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second, // embedding ~50ms, chat ~1-5s, запас на тяжёлые промпты
		},
		embedModel: embedModel,
		chatModel:  chatModel,
	}
}

// ════════════════════════════════════════════════
// Структуры запросов/ответов Ollama API
// ════════════════════════════════════════════════
//
// Ollama общается JSON-ом. Нам нужно описать формат.
// Это как protocol.Value в твоём RESP — только для HTTP+JSON.

// embedRequest — тело POST /api/embed
type embedRequest struct {
	Model string `json:"model"` // "nomic-embed-text"
	Input string `json:"input"` // текст для embedding
}

// embedResponse — ответ POST /api/embed
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"` // массив векторов (обычно 1)
}

// chatRequest — тело POST /api/chat
type chatRequest struct {
	Model    string        `json:"model"` // "gemma4:e2b"
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"` // false = ждём полный ответ
}

type chatMessage struct {
	Role    string `json:"role"`    // "user" или "assistant"
	Content string `json:"content"` // текст сообщения
}

// chatResponse — ответ POST /api/chat
type chatResponse struct {
	Message chatMessage `json:"message"`
}

// ════════════════════════════════════════════════
// Методы клиента
// ════════════════════════════════════════════════

// Embed превращает текст в вектор.
//
// Это КЛЮЧЕВАЯ функция всей AI-интеграции.
// Текст "Кошки спят 16 часов" → [0.12, -0.34, 0.56, ...] (768 чисел)
//
// Зачем context.Context?
// Тот же паттерн, что в WASM Engine (ExecFunction):
// позволяет вызывающему коду отменить запрос по таймауту.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	// 1. Формируем JSON-запрос
	reqBody := embedRequest{
		Model: c.embedModel,
		Input: text,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	// 2. Создаём HTTP-запрос с контекстом
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 3. Отправляем
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request failed: %w", err)
	}
	defer resp.Body.Close()

	// 4. Проверяем статус
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// 5. Парсим ответ
	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}

	// 6. Ollama возвращает массив embedding-ов (для batch).
	//    Мы отправляем 1 текст → получаем 1 вектор.
	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("empty embeddings response")
	}

	return result.Embeddings[0], nil
}

// Chat отправляет промпт в LLM и получает ответ.
//
// stream=false — ждём полный ответ.
// Для v1 это проще: не нужен streaming parser.
// В будущем можно добавить streaming через PubSub
// (каждый токен → PUBLISH ai:stream:requestID token).
func (c *Client) Chat(ctx context.Context, prompt string) (string, error) {
	reqBody := chatRequest{
		Model: c.chatModel,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama chat error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}

	return result.Message.Content, nil
}

// Ping проверяет доступность Ollama.
//
// Вызывается при старте KVStore.
// Если Ollama недоступна — AI-команды отключаются,
// но всё остальное работает (graceful degradation).
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama not reachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	return nil
}
