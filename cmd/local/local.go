package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/susocks/susocks"
	"github.com/susocks/susocks/model"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	u               *url.URL
	Conns           map[*websocket.Conn]struct{}
	mutex           *sync.Mutex
	UserWriteChans  map[string]chan model.Pack
	ServerReadChan  chan model.Pack
	ServerWriteChan chan model.Pack
}

func NewConnPool(url2 *url.URL) *ConnPool {
	connPool := &ConnPool{
		Conns:           make(map[*websocket.Conn]struct{}),
		mutex:           new(sync.Mutex),
		u:               url2,
		UserWriteChans:  make(map[string]chan model.Pack),
		ServerReadChan:  make(chan model.Pack, 10),
		ServerWriteChan: make(chan model.Pack, 10),
	}
	connPoolID := time.Now().UnixNano()
	for i := 0; i < *worker; i++ {
		_, err := connPool.NewConn(url2, strconv.FormatInt(connPoolID, 10))
		if err != nil {
			log.Print(err.Error())
		}
	}
	go func() {
		for {
			select {
			case data := <-connPool.ServerReadChan:
				//log.Print("got server resp:", data.Addr, data.Data)
				func() {
					connPool.mutex.Lock()
					ch, ok := connPool.UserWriteChans[data.Addr]
					connPool.mutex.Unlock()
					if ok {
						select {
						case ch <- data:
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

func (connPool *ConnPool) NewConn(u *url.URL, connPoolID string) (*websocket.Conn, error) {
	log.Printf("connecting to %s", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(),
		http.Header{
			"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(*basicAuth))},
			"Connpoolid":    []string{connPoolID},
		})
	if err != nil {
		log.Print("dial:", err.Error())
		return nil, err
	}
	log.Printf("connected to %s", u.String())
	err = c.SetCompressionLevel(6)
	if err != nil {
		log.Print("SetCompressionLevel:", err.Error())
	}
	c.EnableWriteCompression(true)
	ctx, cancelFunc := context.WithCancel(context.Background())
	go func() { // server write
		for {
			select {
			case <-ctx.Done():
				connPool.mutex.Lock()
				delete(connPool.Conns, c)
				connPool.mutex.Unlock()
				return
			case pack := <-connPool.ServerWriteChan:
				data, err := proto.Marshal(&pack)
				if err != nil {
					log.Print(err.Error())
					continue
				}
				err = c.WriteMessage(websocket.BinaryMessage, data)
				if err != nil {
					log.Print(err.Error())
					continue
				}
			case <-time.After(time.Second * 30):
				err = c.WriteMessage(websocket.PingMessage, nil)
				if err != nil {
					log.Print(err.Error())
					continue
				}

			}
		}
	}()
	go func() { // server read
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				log.Print(err.Error())
				cancelFunc()
				return
			}
			//log.Print("got server data:", mt, data)
			switch mt {
			case websocket.PingMessage, websocket.PongMessage:
			case websocket.BinaryMessage:
				message := model.Pack{}
				err := proto.Unmarshal(data, &message)
				if err != nil {
					log.Print(err.Error())
					continue
				}
				connPool.ServerReadChan <- message
			case websocket.CloseMessage:
				log.Print(err.Error())
				cancelFunc()
				return
			default:
				log.Print("unkown mt:")
			}
		}
	}()
	connPool.Put(c)
	return c, nil
}

func (connPool *ConnPool) Listen(addr2 string, scChan chan model.Pack) {
	connPool.mutex.Lock()
	connPool.UserWriteChans[addr2] = scChan
	connPool.mutex.Unlock()
}

func (connPool *ConnPool) Send(addr2 net.Addr, index int64, data []byte) error {
	pack := model.Pack{
		Addr:  addr2.String(),
		Index: index,
		Data:  data,
	}
	select {
	case connPool.ServerWriteChan <- pack:
	case <-time.After(time.Second * 10):
		return errors.New("timeout")
	}
	return nil
}

func handleSocks5Header(conn net.Conn) error {
	// Read the version byte
	version := []byte{0}
	if _, err := conn.Read(version); err != nil {
		log.Printf("[ERR] socks: Failed to get version byte: %v", err)
	}
	// Ensure we are compatible
	if version[0] != susocks.Socks5Version {
		err := fmt.Errorf("Unsupported SOCKS version: %v", version)
		log.Printf("[ERR] socks: %v", err)
	}
	_, err := susocks.ReadMethods(conn)
	if err != nil {
		return err
	}
	_, err = conn.Write([]byte{susocks.Socks5Version, susocks.NoAuth})
	if err != nil {
		return err
	}
	return nil
}

func process(connPool *ConnPool, conn net.Conn) {
	log.Print("new request from ", conn.RemoteAddr())
	err := handleSocks5Header(conn)
	if err != nil {
		log.Print(err.Error())
		return
	}
	var serverWriteIndex int64
	var serverReadExpectIndex int64
	ServerReadWaitMap := make(map[int64]*model.Pack)
	ctx, cancelFunc := context.WithCancel(context.Background())
	csChan := make(chan []byte, 1)
	scChan := make(chan model.Pack, 1)
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
			//log.Print("got user req:", conn.RemoteAddr().String(), data[:n])
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
				connPool.Send(conn.RemoteAddr(), serverWriteIndex, []byte{})
				conn.Close()
				return
			case data := <-csChan:
				err := connPool.Send(conn.RemoteAddr(), serverWriteIndex, data)
				if err != nil {
					cancelFunc()
					log.Print(err.Error())
					return
				}
				serverWriteIndex += 1
			case data := <-scChan:
				ServerReadWaitMap[data.Index] = &data
				for {
					pack, ok := ServerReadWaitMap[serverReadExpectIndex]
					if ok {
						delete(ServerReadWaitMap, serverReadExpectIndex)
						serverReadExpectIndex += 1
						_, err := conn.Write(pack.Data)
						if err != nil {
							cancelFunc()
							log.Print(err.Error())
							return
						}
					} else {
						break
					}
				}
			}
		}
	}()
}
