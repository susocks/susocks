package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/susocks/susocks"
	"github.com/susocks/susocks/model"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

var rAddr net.Addr
var addr = flag.String("addr", "0.0.0.0:8080", "http service address")
var basicAuth = flag.String("auth", "user:password", "basic auth")
var poolMap = make(map[string]*ConnPool)
var poolMapMutex = new(sync.Mutex)

func main() {
	var err error
	flag.Parse()
	rAddr, err = GetIntranetIp()
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/susocks", handler)
	log.Print("listenning: ", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handler(responseWriter http.ResponseWriter, request *http.Request) {
	log.Print("got connect from ", request.RemoteAddr)
	log.Print(request.Header)
	auths := request.Header["Authorization"]
	if len(auths) != 1 || auths[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte(*basicAuth)) {
		log.Print("auth failed")
		responseWriter.WriteHeader(403)
		responseWriter.Write([]byte{})
		return
	}
	log.Print("auth success")
	if len(request.Header["Connpoolid"]) != 1 {
		log.Print("Connpoolid error", request.Header["ConnPoolID"])
		responseWriter.WriteHeader(400)
		responseWriter.Write([]byte{})
		return
	}
	connPoolID := request.Header["Connpoolid"][0]
	poolMapMutex.Lock()
	pool, ok := poolMap[connPoolID]
	if !ok {
		pool = NewServe(connPoolID)
		poolMap[connPoolID] = pool
	}
	poolMapMutex.Unlock()
	pool.susocksHandler(responseWriter, request)
}

type ConnPool struct {
	ID    string
	Conns map[*websocket.Conn]struct{}
	Socks map[string]chan []byte
	mutex *sync.Mutex
}

func NewServe(id string) *ConnPool {
	return &ConnPool{
		ID:    id,
		Conns: make(map[*websocket.Conn]struct{}),
		Socks: make(map[string]chan []byte, 1),
		mutex: new(sync.Mutex),
	}
}

func (connPool *ConnPool) Put(conn *websocket.Conn) {
	connPool.mutex.Lock()
	connPool.Conns[conn] = struct{}{}
	connPool.mutex.Unlock()
}
func (connPool *ConnPool) Send(addr2 string, data []byte) error {
	pack := &model.Pack{
		Addr: addr2,
		Data: data,
	}
	message, err := proto.Marshal(pack)
	if err != nil {
		log.Print(err.Error())
		return err
	}
	connPool.mutex.Lock()
	defer connPool.mutex.Unlock()
	for conn := range connPool.Conns {
		err = conn.WriteMessage(websocket.BinaryMessage, message)
		if err != nil {
			log.Print(err.Error())
			return err
		}
		return nil
	}
	return nil
}

func (connPool *ConnPool) susocksHandler(responseWriter http.ResponseWriter, request *http.Request) {
	ws := &websocket.Upgrader{}
	c, err := ws.Upgrade(responseWriter, request, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()
	c.SetCompressionLevel(6)
	c.EnableWriteCompression(true)
	connPool.Put(c)
	err = connPool.ServeWebSocks(c)
	if err != nil {
		log.Print(err.Error())
		return
	}
}

func (connPool *ConnPool) ServeWebSocks(conn *websocket.Conn) error {
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			log.Print(err.Error())
			return err
		}
		switch mt {
		case websocket.PingMessage:
			err = conn.WriteMessage(websocket.PongMessage, nil)
			if err != nil {
				log.Print(err.Error())
			}
		case websocket.BinaryMessage:
			message := model.Pack{}
			err := proto.Unmarshal(data, &message)
			if err != nil {
				log.Print(err.Error())
				continue
			}
			log.Print("got user req:", message.Addr, message.Data)
			connPool.mutex.Lock()
			ch, ok := connPool.Socks[message.Addr]
			connPool.mutex.Unlock()
			if !ok {
				ch = make(chan []byte)
				socks := NewSocks(connPool, message.Addr, ch, conn.RemoteAddr())
				conf, err := susocks.New(&susocks.Config{})
				if err != nil {
					log.Print(err.Error())
					continue
				}
				connPool.mutex.Lock()
				connPool.Socks[message.Addr] = ch
				connPool.mutex.Unlock()
				go conf.ServeConn(socks)
				log.Print("got request from:", message.Addr)
			}
			go func() {
				select {
				case <-time.After(time.Second * 5):
				case ch <- message.Data:
				}
			}()
		}
	}
	return nil
}

func NewSocks(connPool *ConnPool, addr string, ch chan []byte, remoteAddr net.Addr) *Socks {
	socks := &Socks{
		remoteAddr: remoteAddr,
		addr:       addr,
		connPool:   connPool,
		userChan:   ch,
	}
	socks.ctx, socks.cancanFunc = context.WithCancel(context.Background())
	return socks
}

type Socks struct {
	remoteAddr net.Addr
	addr       string
	connPool   *ConnPool
	userChan   chan []byte
	ctx        context.Context
	cancanFunc context.CancelFunc
}

func (socks *Socks) Read(b []byte) (n int, err error) {
	select {
	case <-socks.ctx.Done():
		return 0, io.EOF
	case <-time.After(time.Minute * 5):
		return 0, io.EOF
	case message := <-socks.userChan:
		for i, m := range message {
			b[i] = m
		}
		return len(message), nil
	}
}

func (socks *Socks) Write(b []byte) (n int, err error) {
	//log.Print("(socks Socks) Write(b []byte)")
	err = socks.connPool.Send(socks.addr, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (socks *Socks) Close() error {
	err := socks.connPool.Send(socks.addr, []byte{})
	if err != nil {
		log.Print(err.Error())
	}
	delete(socks.connPool.Socks, socks.addr)
	return err
}

func (socks *Socks) RemoteAddr() net.Addr {
	return socks.remoteAddr
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
