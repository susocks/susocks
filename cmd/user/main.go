package main

import (
	"if.run/tuotoo/utils"
	"net"
)

func main() {
	conn, err := net.Dial("tcp", "127.0.0.1:1080")
	if !utils.Check(err) {
		return
	}
	utils.Debug("writing 5 1 0")
	_, err = conn.Write([]byte{5, 1, 0})
	if !utils.Check(err) {
		return
	}
	utils.Debug("writed 5 1 0")
	utils.Debug("reading")
	data := make([]byte, 4096)
	n, err := conn.Read(data)
	if !utils.Check(err) {
		return
	}
	utils.Debug("read", n, data[:n])
}
