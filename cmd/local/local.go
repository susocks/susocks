package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"github.com/gorilla/websocket"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
)

const (
	IDLE = 6
)

var addr = flag.String("url", "wss://susocks.if.run/susocks", "susocks url")
var bind = flag.String("bind", "127.0.0.1:1080", "socks5 bind ip:port")
var basicAuth = flag.String("auth", "user:password", "basic auth")

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

func main() {
	flag.Parse()

	//interrupt := make(chan os.Signal, 1)
	//signal.Notify(interrupt, os.Interrupt)

	u, err := url.Parse(*addr)
	if err != nil {
		log.Fatal("parse url:", err)
	}

	listener, err := net.Listen("tcp", *bind)
	if err != nil {
		fmt.Printf("listen fail, err: %v\n", err)
		return
	}
	connPoll := NewConnPool(u)
	//2.accept client request
	//3.create goroutine for each request
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("accept fail, err: %v\n", err)
			continue
		}

		//create goroutine for each connect
		go process(connPoll.Popup(), conn)
	}
}

type ConnPool struct {
	u     *url.URL
	Conns []*websocket.Conn
	mutex *sync.Mutex
}

func NewConnPool(url2 *url.URL) *ConnPool {
	pool := &ConnPool{Conns: []*websocket.Conn{}, mutex: new(sync.Mutex), u: url2}
	for i := 0; i < 10; i++ {
		go pool.NewConn()
	}
	return pool
}

func (connPool *ConnPool) Popup() *websocket.Conn {
	connPool.mutex.Lock()
	if len(connPool.Conns) == 0 {
		connPool.mutex.Unlock()
		conn, err := NewConn(connPool.u)
		if err != nil {
			return nil
		}
		return conn
	}
	first := connPool.Conns[0]
	connPool.Conns = connPool.Conns[1:]
	connPool.mutex.Unlock()
	go connPool.NewConn()
	return first
}

func (connPool *ConnPool) NewConn() error {
	log.Printf("connecting to %s", connPool.u.String())
	c, _, err := websocket.DefaultDialer.Dial(connPool.u.String(),
		http.Header{"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(*basicAuth))}})
	if err != nil {
		log.Print("dial:", err.Error())
		return err
	}
	connPool.mutex.Lock()
	defer connPool.mutex.Unlock()
	connPool.Conns = append(connPool.Conns, c)
	return nil
}

func NewConn(u *url.URL) (*websocket.Conn, error) {
	log.Printf("connecting to %s", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(),
		http.Header{"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(*basicAuth))}})
	if err != nil {
		log.Print("dial:", err.Error())
		return nil, err
	}
	return c, nil
}

func process(c *websocket.Conn, conn net.Conn) {
	if c == nil {
		log.Print("websocket.Conn is nil")
		conn.Close()
		return
	}
	//defer c.Close()
	ctx, cancelFunc := context.WithCancel(context.Background())
	csChan := make(chan []byte, 1)
	scChan := make(chan []byte, 1)
	go func() {
		for {
			data := make([]byte, 4096)
			n, err := conn.Read(data)
			if err != nil {
				cancelFunc()
				log.Print(err)
				return
			}
			select {
			case csChan <- data[:n]:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				cancelFunc()
				log.Print(err.Error())
				return
			}
			select {
			case scChan <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				c.Close()
				conn.Close()
				return
			case data := <-csChan:
				err := c.WriteMessage(websocket.BinaryMessage, data)
				if err != nil {
					cancelFunc()
					log.Print(err.Error())
					return
				}
			case data := <-scChan:
				_, err := conn.Write(data)
				if err != nil {
					cancelFunc()
					log.Print(err.Error())
					return
				}
			}
		}
	}()
}
