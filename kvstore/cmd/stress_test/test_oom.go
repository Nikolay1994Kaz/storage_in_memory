package main

import (
	"fmt"
	"net"
	"strings"

	"kvstore/kvstore/internal/protocol"
)

func sendCommand(w *protocol.Writer, args ...string) error {
	arr := make([]protocol.Value, len(args))
	for i, a := range args {
		arr[i] = protocol.Value{Typ: '$', Str: a}
	}
	if err := w.Write(protocol.Value{Typ: '*', Array: arr}); err != nil {
		return err
	}
	return w.Flush()
}

func main() {
	conn, err := net.Dial("tcp", "localhost:6389")
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)

	largeVal := strings.Repeat("A", 100*1024)

	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("oom_check:%d", i)
		err := sendCommand(w, "SET", key, largeVal)
		if err != nil {
			fmt.Printf("Error sending at %d: %v\n", i, err)
			return
		}

		res, err := r.Read()
		if err != nil {
			fmt.Printf("Error reading at %d: %v\n", i, err)
			return
		}

		if res.Typ == '-' {
			fmt.Printf("Received error at %d: %s\n", i, res.Str)
			return
		}
	}
	fmt.Println("Done writing 200 keys of 100KB without any OOM error!")
}
