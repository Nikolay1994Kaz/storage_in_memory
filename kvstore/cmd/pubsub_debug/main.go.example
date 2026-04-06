package main

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/protocol"
)

func sendCommand(w *protocol.Writer, args ...string) error {
	arr := make([]protocol.Value, len(args))
	for i, a := range args {
		arr[i] = protocol.Value{Typ: '$', Str: a}
	}
	return w.Write(protocol.Value{Typ: '*', Array: arr})
}

var programStart = time.Now()

func ts() string {
	return fmt.Sprintf("[+%6.1fms]", float64(time.Since(programStart).Microseconds()/1000))
}

func main() {
	addr := "localhost:6380"
	channel := "debug:pubsub"
	numSubscribers := 2 
	numMessages := 3 

	fmt.Println("=== Pub/Sub Debug: пошаговый разбор ===")
	fmt.Printf("Подписчиков: %d, Сообщений: %d\n\n", numSubscribers, numMessages)
	var received atomic.Int64
	var wg sync.WaitGroup
	fmt.Printf("%s ШАГ 1: Запускаем %d подписчиков\n", ts(), numSubscribers)
	for i := 0; i < numSubscribers; i++ {
		wg.Add(1)
		go func(subID int) {
			defer wg.Done()
			fmt.Printf("%s  [sub-%d] Подключаюсь к %s...\n", ts(), subID, addr)
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				fmt.Printf("%s   [sub-%d] ОШИБКА подключения: %v\n", ts(), subID, err)
				return
			}
			defer conn.Close()
			fmt.Printf("%s   [sub-%d] ✓ Подключен (fd: TCP conn установлен)\n", ts(), subID)
			wirter := protocol.NewWriter(conn)
			reader := protocol.NewReader(conn)
			
		}
	}
}