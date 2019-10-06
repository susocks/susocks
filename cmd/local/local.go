package main

import (
	"context"
	"crypto/md5"
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

var addr = flag.String("url", "ws://localhost:8080/susocks", "susocks url")
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
	wsPoll := NewWSPool(u)
	//2.accept client request
	//3.create goroutine for each request
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("accept fail, err: %v\n", err)
			continue
		}

		//create goroutine for each connect
		go ProcessUserConn(wsPoll, conn)
	}
}

type WSPool struct {
	u              *url.URL
	WSs            map[*websocket.Conn]struct{}
	mutex          *sync.Mutex
	UserWriteChans map[string]chan model.Pack
	WSReadChan     chan model.Pack
	WSWriteChan    chan model.Pack
}

func NewWSPool(url2 *url.URL) *WSPool {
	wsPool := &WSPool{
		WSs:            make(map[*websocket.Conn]struct{}),
		mutex:          new(sync.Mutex),
		u:              url2,
		UserWriteChans: make(map[string]chan model.Pack),
		WSReadChan:     make(chan model.Pack, 10),
		WSWriteChan:    make(chan model.Pack, 10),
	}
	connPoolID := time.Now().UnixNano()
	for i := 0; i < *worker; i++ {
		_, err := wsPool.NewConn(url2, strconv.FormatInt(connPoolID, 10))
		if err != nil {
			log.Print(err.Error())
		}
	}
	go func() {
		for {
			select {
			case data := <-wsPool.WSReadChan:
				//log.Print("got server resp:", data.Addr, data.Data)
				func() {
					wsPool.mutex.Lock()
					ch, ok := wsPool.UserWriteChans[data.Addr]
					wsPool.mutex.Unlock()
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
	return wsPool
}

func (wsPoll *WSPool) Put(conn *websocket.Conn) {
	wsPoll.mutex.Lock()
	wsPoll.WSs[conn] = struct{}{}
	wsPoll.mutex.Unlock()
}

func (wsPoll *WSPool) NewConn(u *url.URL, connPoolID string) (*websocket.Conn, error) {
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
				wsPoll.mutex.Lock()
				delete(wsPoll.WSs, c)
				wsPoll.mutex.Unlock()
				return
			case pack := <-wsPoll.WSWriteChan:
				data, err := proto.Marshal(&pack)
				if err != nil {
					log.Print(err.Error())
					continue
				}
				log.Print("WSWriteIndex:", pack.Index, pack.Md5)
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
				wsPoll.WSReadChan <- message
			case websocket.CloseMessage:
				log.Print(err.Error())
				cancelFunc()
				return
			default:
				log.Print("unkown mt:")
			}
		}
	}()
	wsPoll.Put(c)
	return c, nil
}

func (wsPoll *WSPool) Listen(addr2 string, scChan chan model.Pack) {
	wsPoll.mutex.Lock()
	wsPoll.UserWriteChans[addr2] = scChan
	wsPoll.mutex.Unlock()
}

func (wsPoll *WSPool) Send(addr2 net.Addr, index int64, data []byte) error {
	pack := model.Pack{
		Addr:  addr2.String(),
		Index: index,
		Data:  data,
		Md5:   fmt.Sprintf("%x", md5.Sum(data)),
	}
	select {
	case wsPoll.WSWriteChan <- pack:
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

func ProcessUserConn(wsPool *WSPool, userConn net.Conn) {
	log.Print("new user request from ", userConn.RemoteAddr())
	err := handleSocks5Header(userConn)
	if err != nil {
		log.Print(err.Error())
		return
	}
	var WSWriteIndex int64
	var WSReadExpectIndex int64
	WSReadWaitMap := make(map[int64]*model.Pack)
	ctx, cancelFunc := context.WithCancel(context.Background())
	csChan := make(chan []byte, 1)
	scChan := make(chan model.Pack, 1)
	wsPool.Listen(userConn.RemoteAddr().String(), scChan)
	go func() { // user -> csChan
		for {
			data := make([]byte, 4096)
			n, err := userConn.Read(data)
			if err != nil {
				log.Print("cancelFunc()", err.Error())
				cancelFunc()
				log.Print("userConn Closing", userConn.RemoteAddr())
				err = userConn.Close()
				if err != nil {
					log.Print("userConn Closed failed", err.Error())
					break
				}
				log.Print("userConn Closed", userConn.RemoteAddr())
				break
			}
			//log.Print("got user req:", userConn.RemoteAddr().String(), data[:n])
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
				log.Print("closing socks:", userConn.RemoteAddr())
				err := wsPool.Send(userConn.RemoteAddr(), WSWriteIndex, []byte{})
				if err != nil {
					fmt.Print("close socks failed:", userConn.RemoteAddr(), err.Error())
				}
				log.Print("socks Closed", userConn.RemoteAddr())

				log.Print("userConn Closing", userConn.RemoteAddr())
				err = userConn.Close()
				if err != nil {
					log.Print("userConn Closed failed", err.Error())
					return
				}
				log.Print("userConn Closed", userConn.RemoteAddr())
				return
			case data := <-csChan:
				err := wsPool.Send(userConn.RemoteAddr(), WSWriteIndex, data)
				if err != nil {
					cancelFunc()
					log.Print("cancelFunc()", err.Error())
					log.Print("userConn Closing", userConn.RemoteAddr())
					err = userConn.Close()
					if err != nil {
						log.Print("userConn Closed failed", err.Error())
						break
					}
					log.Print("userConn Closed", userConn.RemoteAddr())
					break
				}
				WSWriteIndex += 1
			case data := <-scChan:
				WSReadWaitMap[data.Index] = &data
				if len(data.Data) == 0 {
					cancelFunc()
					log.Print("cancelFunc()")
					log.Print("userConn Closing", userConn.RemoteAddr())
					err = userConn.Close()
					if err != nil {
						log.Print("userConn Closed failed", err.Error())
						break
					}
					log.Print("userConn Closed", userConn.RemoteAddr())
					break
				}
				for {
					pack, ok := WSReadWaitMap[WSReadExpectIndex]
					if ok {
						delete(WSReadWaitMap, WSReadExpectIndex)
						WSReadExpectIndex += 1
						_, err := userConn.Write(pack.Data)
						if err != nil {
							cancelFunc()
							log.Print("cancelFunc()", err.Error())
							break
						}
					} else {
						if len(WSReadWaitMap) > 0 {
							log.Print("expect:", WSReadExpectIndex)
							for _, v := range WSReadWaitMap {
								log.Print("have:", v.Index)
							}
						}
						break
					}
				}
			}
		}
	}()
}
