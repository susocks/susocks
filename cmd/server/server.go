package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"github.com/gorilla/websocket"
	"github.com/susocks/susocks"
	"log"
	"net"
	"net/http"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

var rAddr net.Addr
var addr = flag.String("addr", "0.0.0.0:80", "http service address")
var basicAuth = flag.String("auth", "user:password", "basic auth")

func main() {
	var err error
	flag.Parse()
	rAddr, err = GetIntranetIp()
	if err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("/susocks", susocksHandler)
	log.Print("listenning: ", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func susocksHandler(responseWriter http.ResponseWriter, request *http.Request) {
	log.Print("got request from ", request.RemoteAddr)
	auths := request.Header["Authorization"]
	if len(auths) != 1 || auths[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte(*basicAuth)) {
		log.Print("auth failed")
		responseWriter.WriteHeader(403)
		responseWriter.Write([]byte{})
		return
	}
	log.Print("auth success")
	ws := &websocket.Upgrader{}
	c, err := ws.Upgrade(responseWriter, request, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	c.SetCompressionLevel(6)
	c.EnableWriteCompression(true)
	s, err := susocks.New(&susocks.Config{})
	if err != nil {
		log.Print(err.Error())
		return
	}
	err = s.ServeConn(SConn{Conn: c, RAddr: rAddr})
	if err != nil {
		log.Print(err.Error())
		return
	}
}

type SConn struct {
	Conn  *websocket.Conn
	RAddr net.Addr
}

func (conn SConn) Read(b []byte) (n int, err error) {
	//log.Print("(conn SConn) Read(b []byte)")
	mt, message, err := conn.Conn.ReadMessage()
	if err != nil {
		log.Print(err.Error())
		return 0, err
	}
	if mt == websocket.PingMessage {
		err = conn.Conn.WriteMessage(websocket.PongMessage, nil)
		if err != nil {
			log.Print(err.Error())
		}
		return conn.Read(b)
	}
	//log.Print("read message:",message)
	for i, m := range message {
		b[i] = m
	}
	//log.Print("b",b[:len(message)])
	return len(message), nil
}

func (conn SConn) Write(b []byte) (n int, err error) {
	//log.Print("(conn SConn) Write(b []byte)")
	err = conn.Conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (conn SConn) Close() error {
	return conn.Conn.Close()
}

func (conn SConn) RemoteAddr() net.Addr {
	return conn.RAddr
}

func GetIntranetIp() (net.Addr, error) {
	addrs, err := net.InterfaceAddrs()

	if err != nil {
		return nil, err
	}

	for _, address := range addrs {

		// 检查ip地址判断是否回环地址
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return address, nil
			}

		}
	}
	return nil, errors.New("not found")
}
