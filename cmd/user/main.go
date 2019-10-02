package main

import (
	"net"
)

func main() {
	conn, err := net.Dial("tcp", "127.0.0.1:1080")
	if err != nil {
		return
	}
	_, err = conn.Write([]byte{5, 1, 0})
	if err != nil {
		return
	}
	data := make([]byte, 4096)
	n, err := conn.Read(data)
	if err != nil {
		return
	}
}
