package main

import "testing"

// AI.*-семейство: в харнессе aiClient/aiWorker = nil (как в проде, когда Ollama
// недоступен — ai.NewClient().Ping() провалился) → все команды обязаны отвечать
// внятной ошибкой «Ollama not available», а не паниковать на nil-дереференсе.
// Это законный прод-путь (Ollama — рантайм-фича, не build-tag).
func TestExec_AiCommandsWithoutOllama(t *testing.T) {
	e := newExecEnv(t)

	// aiClient-гейт.
	e.wantErrPrefix(e.do("AI.EMBED", "hello"), "ERR Ollama not available")
	e.wantErrPrefix(e.do("AI.SEARCH", "5", "query"), "ERR Ollama not available")
	e.wantErrPrefix(e.do("AI.ASK", "why?"), "ERR Ollama not available")
	// aiWorker-гейт (отдельная nil-проверка).
	e.wantErrPrefix(e.do("AI.INGEST", "k", "text"), "ERR Ollama not available")
}
