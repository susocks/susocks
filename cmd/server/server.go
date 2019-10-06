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
	auths := request.Header["Authorization"]
	if len(auths) != 1 || auths[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte(*basicAuth)) {
		log.Print("auth failed")
		responseWriter.WriteHeader(403)
		responseWriter.Write([]byte{})
		return
	}
	log.Print("connected with ", request.RemoteAddr)
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
	ID              string
	Conns           map[*websocket.Conn]struct{}
	Socks           map[string]chan []byte
	mutex           *sync.Mutex
	ServerWriteChan chan []byte
}

func NewServe(id string) *ConnPool {
	return &ConnPool{
		ID:              id,
		Conns:           make(map[*websocket.Conn]struct{}),
		Socks:           make(map[string]chan []byte),
		mutex:           new(sync.Mutex),
		ServerWriteChan: make(chan []byte, 10),
	}
}

func (connPool *ConnPool) Put(conn *websocket.Conn) {
	connPool.mutex.Lock()
	connPool.Conns[conn] = struct{}{}
	connPool.mutex.Unlock()
}
func (connPool *ConnPool) Send(pack *model.Pack) error {
	message, err := proto.Marshal(pack)
	if err != nil {
		log.Print(err.Error())
		return err
	}
	select {
	case connPool.ServerWriteChan <- message:
	case <-time.After(time.Second * 10):
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
	go func() {
		for {
			select {
			case message := <-connPool.ServerWriteChan:
				err := conn.WriteMessage(websocket.BinaryMessage, message)
				if err != nil {
					log.Print(err.Error())
					return
				}
			}
		}
	}()
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
			log.Print("got user req:", message.Addr)
			connPool.mutex.Lock()
			ch, ok := connPool.Socks[message.Addr]
			if !ok {
				ch = make(chan []byte)
				socks := NewSocks(connPool, message.Addr, ch, conn.RemoteAddr())
				conf, err := susocks.New(&susocks.Config{})
				if err != nil {
					log.Print(err.Error())
					continue
				}
				connPool.Socks[message.Addr] = ch
				go conf.ServeConn(socks)
				log.Print("got websocket from:", message.Addr)
			}
			connPool.mutex.Unlock()
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
		mutex:      new(sync.Mutex),
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
	index      int64
	mutex      *sync.Mutex
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

func (socks *Socks) Index() int64 {
	socks.mutex.Lock()
	defer socks.mutex.Unlock()
	i := socks.index
	socks.index += 1
	return i
}

func (socks *Socks) Write(b []byte) (n int, err error) {
	pack := &model.Pack{
		Addr:  socks.addr,
		Data:  b,
		Index: socks.Index(),
	}
	err = socks.connPool.Send(pack)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (socks *Socks) Close() error {
	pack := &model.Pack{
		Addr:  socks.addr,
		Data:  []byte{},
		Index: socks.Index(),
	}
	err := socks.connPool.Send(pack)
	if err != nil {
		log.Print(err.Error())
	}
	socks.connPool.mutex.Lock()
	delete(socks.connPool.Socks, socks.addr)
	socks.connPool.mutex.Unlock()
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
