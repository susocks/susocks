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
var poolMap = make(map[string]*WSPool)
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
		pool = NewWSPool(connPoolID)
		poolMap[connPoolID] = pool
	}
	poolMapMutex.Unlock()
	pool.susocksHandler(responseWriter, request)
}

type WSPool struct {
	ID              string
	WSs             map[*websocket.Conn]struct{}
	WSReadChan      map[string]chan model.Pack
	mutex           *sync.Mutex
	ServerWriteChan chan []byte
}

func NewWSPool(id string) *WSPool {
	return &WSPool{
		ID:              id,
		WSs:             make(map[*websocket.Conn]struct{}),
		WSReadChan:      make(map[string]chan model.Pack),
		mutex:           new(sync.Mutex),
		ServerWriteChan: make(chan []byte, 10),
	}
}

func (wsPool *WSPool) Put(conn *websocket.Conn) {
	wsPool.mutex.Lock()
	wsPool.WSs[conn] = struct{}{}
	wsPool.mutex.Unlock()
}

func (wsPool *WSPool) Remove(conn *websocket.Conn) {
	wsPool.mutex.Lock()
	delete(wsPool.WSs, conn)
	if len(wsPool.WSs) == 0 {
		poolMapMutex.Lock()
		delete(poolMap, wsPool.ID)
		poolMapMutex.Unlock()
	}
	wsPool.mutex.Unlock()
}

func (wsPool *WSPool) Send(pack *model.Pack) error {
	message, err := proto.Marshal(pack)
	if err != nil {
		log.Print(err.Error())
		return err
	}
	select {
	case wsPool.ServerWriteChan <- message:
	case <-time.After(time.Second * 10):
	}
	return nil
}

func (wsPool *WSPool) susocksHandler(responseWriter http.ResponseWriter, request *http.Request) {
	ws := &websocket.Upgrader{}
	c, err := ws.Upgrade(responseWriter, request, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer func() {
		c.Close()
		wsPool.Remove(c)
	}()
	c.SetCompressionLevel(6)
	c.EnableWriteCompression(true)
	wsPool.Put(c)
	err = wsPool.ServeWebSocks(c)
	if err != nil {
		log.Print(err.Error())
		return
	}
}

func (wsPool *WSPool) ServeWebSocks(ws *websocket.Conn) error {
	go func() {
		for {
			select {
			case message := <-wsPool.ServerWriteChan:
				err := ws.WriteMessage(websocket.BinaryMessage, message)
				if err != nil {
					log.Print(err.Error())
					return
				}
			}
		}
	}()
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			log.Print(err.Error())
			return err
		}
		switch mt {
		case websocket.PingMessage:
			err = ws.WriteMessage(websocket.PongMessage, nil)
			if err != nil {
				log.Print(err.Error())
			}
		case websocket.BinaryMessage:
			message := &model.Pack{}
			err := proto.Unmarshal(data, message)
			if err != nil {
				log.Print(err.Error())
				continue
			}
			wsPool.mutex.Lock()
			ch, ok := wsPool.WSReadChan[message.Addr]
			if !ok {
				log.Print("got user req:", message.Addr)
				ch = make(chan model.Pack)
				socks := NewSocks(wsPool, message.Addr, ch, ws.RemoteAddr())
				conf, err := susocks.New(&susocks.Config{})
				if err != nil {
					log.Print(err.Error())
					continue
				}
				wsPool.WSReadChan[message.Addr] = ch
				go conf.ServeConn(socks)
				log.Print("got ws pack from:", message.Addr)
			}
			wsPool.mutex.Unlock()
			go func() {
				select {
				case <-time.After(time.Second * 5):
				case ch <- *message:
				}
			}()
		}
	}
	return nil
}

func NewSocks(connPool *WSPool, addr string, ch chan model.Pack, remoteAddr net.Addr) *Socks {
	socks := &Socks{
		remoteAddr:        remoteAddr,
		addr:              addr,
		connPool:          connPool,
		userChan:          ch,
		mutex:             new(sync.Mutex),
		ServerReadWaitMap: make(map[int64]model.Pack),
	}
	socks.ctx, socks.cancanFunc = context.WithCancel(context.Background())
	return socks
}

type Socks struct {
	remoteAddr        net.Addr
	addr              string
	connPool          *WSPool
	userChan          chan model.Pack
	ctx               context.Context
	cancanFunc        context.CancelFunc
	wsWriteIndex      int64
	wsReadIndex       int64
	mutex             *sync.Mutex
	ServerReadWaitMap map[int64]model.Pack
}

func (socks *Socks) Read(b []byte) (n int, err error) {
	msg, ok := socks.ServerReadWaitMap[socks.wsReadIndex]
	if ok {
		delete(socks.ServerReadWaitMap, socks.wsReadIndex)
		socks.wsReadIndex += 1
		for i, m := range msg.Data {
			b[i] = m
		}
		return len(msg.Data), nil
	}
	select {
	case <-socks.ctx.Done():
		return 0, io.EOF
	case <-time.After(time.Minute):
		return 0, io.EOF
	case message := <-socks.userChan:
		if len(message.Data) == 0 {
			return 0, io.EOF
		}
		socks.ServerReadWaitMap[message.Index] = message
		return socks.Read(b)
	}
}

func (socks *Socks) WriteIndex() int64 {
	socks.mutex.Lock()
	defer socks.mutex.Unlock()
	i := socks.wsWriteIndex
	socks.wsWriteIndex += 1
	return i
}

func (socks *Socks) Write(b []byte) (n int, err error) {
	pack := &model.Pack{
		Addr:  socks.addr,
		Data:  b,
		Index: socks.WriteIndex(),
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
		Index: socks.WriteIndex(),
	}
	log.Print("closing socks:", socks.RemoteAddr())
	err := socks.connPool.Send(pack)
	if err != nil {
		log.Print(err.Error())
		log.Print("close socks failed:", socks.RemoteAddr())
	}
	log.Print("close socks sended:", socks.RemoteAddr())
	socks.connPool.mutex.Lock()
	delete(socks.connPool.WSReadChan, socks.addr)
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
