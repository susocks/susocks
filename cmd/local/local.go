package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/susocks/susocks/model"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var addr = flag.String("url", "wss://susocks.if.run/susocks", "susocks url")
var bind = flag.String("bind", "127.0.0.1:1080", "socks5 bind ip:port")
var basicAuth = flag.String("auth", "user:password", "basic auth")
var worker = flag.Int("worker", 6, "workers")

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
		go process(connPoll, conn)
	}
}

type ConnPool struct {
	u          *url.URL
	Conns      map[*websocket.Conn]struct{}
	mutex      *sync.Mutex
	UserChans  map[string]chan []byte
	ServerChan chan model.Pack
}

func NewConnPool(url2 *url.URL) *ConnPool {
	connPool := &ConnPool{
		Conns:      make(map[*websocket.Conn]struct{}),
		mutex:      new(sync.Mutex),
		u:          url2,
		UserChans:  make(map[string]chan []byte),
		ServerChan: make(chan model.Pack, 10),
	}
	for i := 0; i < *worker; i++ {
		_, err := connPool.NewConn(url2)
		if err != nil {
			log.Print(err.Error())
		}
	}
	go func() {
		for {
			select {
			case data := <-connPool.ServerChan:
				go func() {
					connPool.mutex.Lock()
					ch, ok := connPool.UserChans[data.Addr]
					connPool.mutex.Unlock()
					if ok {
						select {
						case ch <- data.Data:
						case <-time.After(time.Second * 5):
						}
					}
				}()
			}
		}
	}()
	return connPool
}

func (connPool *ConnPool) Put(conn *websocket.Conn) {
	connPool.mutex.Lock()
	connPool.Conns[conn] = struct{}{}
	connPool.mutex.Unlock()
}

func (connPool *ConnPool) NewConn(u *url.URL) (*websocket.Conn, error) {
	log.Printf("connecting to %s", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(),
		http.Header{"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(*basicAuth))}})
	if err != nil {
		log.Print("dial:", err.Error())
		return nil, err
	}
	err = c.SetCompressionLevel(6)
	if err != nil {
		log.Print("SetCompressionLevel:", err.Error())
	}
	c.EnableWriteCompression(true)
	go func() {
		for {
			time.Sleep(time.Second * 10)
			err := c.WriteMessage(websocket.PingMessage, nil)
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				log.Print(err.Error())
				delete(connPool.Conns, c)
				return
			}
			switch mt {
			case websocket.PingMessage, websocket.PongMessage:
			case websocket.BinaryMessage:
				message := model.Pack{}
				err := proto.Unmarshal(data, &message)
				if err != nil {
					log.Print(err.Error())
					continue
				}
				connPool.ServerChan <- message
			case websocket.CloseMessage:
				log.Print(err.Error())
				delete(connPool.Conns, c)
				return
			default:
				log.Print("unkown mt:")
			}
		}
	}()
	connPool.Put(c)
	return c, nil
}

func (connPool *ConnPool) Listen(addr2 string, scChan chan []byte) {
	connPool.UserChans[addr2] = scChan
}

func (connPool *ConnPool) RandWorker() *websocket.Conn {
	connPool.mutex.Lock()
	for conn := range connPool.Conns {
		connPool.mutex.Unlock()
		return conn
	}
	connPool.mutex.Unlock()
	c, err := connPool.NewConn(connPool.u)
	if err != nil {
		log.Print(err.Error())
		time.Sleep(time.Second)
		return connPool.RandWorker()
	}
	return c
}

func (connPool *ConnPool) Send(addr2 net.Addr, data []byte) error {
	pack := &model.Pack{
		Addr: addr2.String(),
		Data: data,
	}
	message, err := proto.Marshal(pack)
	if err != nil {
		log.Print(err.Error())
		return err
	}
	conn := connPool.RandWorker()
	if conn == nil { // TODO: get alive connect
		return errors.New("conn is not alive")
	}
	err = conn.WriteMessage(websocket.BinaryMessage, message)
	if err != nil {
		log.Print(err.Error())
		return err
	}
	return nil
}

func process(connPool *ConnPool, conn net.Conn) {
	log.Print("got request from ", conn.RemoteAddr())
	ctx, cancelFunc := context.WithCancel(context.Background())
	csChan := make(chan []byte, 1)
	scChan := make(chan []byte, 1)
	connPool.Listen(conn.RemoteAddr().String(), scChan)
	go func() { // user -> csChan
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
			select {
			case <-ctx.Done():
				connPool.Send(conn.RemoteAddr(), []byte{})
				conn.Close()
				return
			case data := <-csChan:
				err := connPool.Send(conn.RemoteAddr(), data)
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
