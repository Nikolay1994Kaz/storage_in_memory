package main

// Команды pub/sub: SUBSCRIBE, UNSUBSCRIBE, PUBLISH.

import (
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
)

// cmdSubscribe — SUBSCRIBE channel…: переводит соединение в режим подписки.
func (c *commands) cmdSubscribe(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 1 {
		buf.WriteError("ERR wrong number of arguments for 'SUBSCRIBE'")
		return
	}
	channels := make([]string, len(args))
	for i, a := range args {
		channels[i] = string(a)
	}
	// M1: subscribeClassic флашит cs.Buf ДО старта writePump — иначе
	// предшествующие в том же пайплайн-батче ответы (напр. +PONG) гонятся
	// за сокет с writePump. writePump отправляет подтверждения, не пишем в buf.
	subscribeClassic(cs, c.hub, channels)
}

// cmdUnsubscribe — UNSUBSCRIBE [channel…].
func (c *commands) cmdUnsubscribe(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	channels := make([]string, len(args))
	for i, a := range args {
		channels[i] = string(a)
	}
	c.hub.Unsubscribe(cs.Conn, channels)
	buf.WriteSimpleString("OK")
}

// cmdPublish — PUBLISH channel message: отвечает числом получателей.
func (c *commands) cmdPublish(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 2 {
		buf.WriteError("ERR wrong number of arguments for 'PUBLISH'")
		return
	}
	count := c.hub.Publish(string(args[0]), string(args[1]))
	buf.WriteInt(count)
}

// flushBeforeWritePump сбрасывает накопленный в cs.Buf вывод на провод ДО того,
// как вызывающий переведёт соединение в subscriber-mode и стартует writePump (M1).
//
// writePump — ВТОРОЙ, независимый писатель того же conn (свой protocol.Writer).
// Пока в cs.Buf лежат ответы предыдущих команд того же пайплайн-батча (напр.
// +PONG на пайплайненный перед SUBSCRIBE PING), они уходят клиенту конечным
// Flush горутины соединения (server.go), который выполняется ПАРАЛЛЕЛЬНО с
// writePump. Два писателя в один сокет без синхронизации → кадры
// переставляются местами:
// клиент, ждущий +PONG следующим ответом, получает subscribe-подтверждение и
// десинхронизирует RESP-поток навсегда. Синхронный Flush здесь гарантирует, что
// весь предшествующий вывод на проводе прежде, чем writePump напишет первый кадр.
// Ошибку глотаем: сломанный conn всё равно будет переиспользован/реапнут (тот же
// Flush повторится в конце батча / writePump упрётся в ошибку записи).
func flushBeforeWritePump(cs *server.ConnState) {
	_ = cs.Buf.Flush()
}

// subscribeClassic переводит соединение в classic subscriber-mode: сперва
// флашит пайплайн-вывод (M1, см. flushBeforeWritePump), затем стартует подписку
// (и её writePump). Порядок «флаш → Subscribe» обязателен: Subscribe пушит
// подтверждение в writePump, который может записать его немедленно из своей
// горутины, поэтому предшествующий вывод должен уйти на провод ДО этого.
func subscribeClassic(cs *server.ConnState, hub *pubsub.Hub, channels []string) {
	flushBeforeWritePump(cs)
	hub.Subscribe(cs.Conn, channels)
}
