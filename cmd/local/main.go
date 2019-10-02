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
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

var addr = flag.String("url", "wss://susocks.if.run/susocks", "susocks url")
var bind = flag.String("bind", "127.0.0.1:1080", "socks5 bind ip:port")
var basicAuth = flag.String("auth", "user:password", "basic auth")

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

	//2.accept client request
	//3.create goroutine for each request
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("accept fail, err: %v\n", err)
			continue
		}

		//create goroutine for each connect
		go process(u, conn)
	}
}

func process(url2 *url.URL, conn net.Conn) {
	log.Printf("connecting to %s", url2.String())

	c, _, err := websocket.DefaultDialer.Dial(url2.String(),
		http.Header{"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(*basicAuth))}})
	if err != nil {
		log.Print("dial:", err.Error())
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
			//utils.Debug("got bytes from user:",data[:n])
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
			//utils.Debug("got bytes from server:",data)
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
				//utils.Debug("sending bytes to server:",data)
				err := c.WriteMessage(websocket.BinaryMessage, data)
				if err != nil {
					cancelFunc()
					log.Print(err.Error())
					return
				}
			case data := <-scChan:
				//utils.Debug("sending bytes to user:",data)
				_, err := conn.Write(data)
				if err != nil {
					cancelFunc()
					log.Print(err.Error())
					return
				}
				//utils.Debug("sended byte",data,n)
			}
		}
	}()
}
