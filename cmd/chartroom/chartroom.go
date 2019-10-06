package main

import (
	"flag"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"
)

var addr = flag.String("addr", "127.0.0.1:8081", "http service address")
var upgrader = websocket.Upgrader{} // use default options
var interrupt = make(chan os.Signal, 1)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

func main() {
	flag.Parse()
	//log.SetFlags(0)
	signal.Notify(interrupt, os.Interrupt)
	http.HandleFunc("/echo", echo)
	go http.ListenAndServe(*addr, nil)
	time.Sleep(time.Second)
	client()
}

func echo(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()
	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			log.Println("server read:", err)
			break
		}
		log.Printf("server recv: %s", message)
		err = c.WriteMessage(mt, message)
		if err != nil {
			log.Println("server write:", err)
			break
		}
		log.Printf("server send: %s", message)
	}
}

func client() {
	u := url.URL{Scheme: "ws", Host: *addr, Path: "/echo"}
	log.Printf("connecting to %s", u.String())
	proxyURL, err := url.Parse("socks5://localhost:1080")
	if err != nil {
		log.Fatal(err)
	}
	c, _, err := (&websocket.Dialer{Proxy: http.ProxyURL(proxyURL)}).Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()

	done := make(chan struct{})
	var m string
	go func() {
		defer close(done)
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("client read:", err)
				return
			}
			log.Printf("client recv: %s", message)
			if m != string(message) {
				log.Fatal("m != string(message)")
			}
		}
	}()

	ticker := time.NewTicker(time.Millisecond * 5)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case t := <-ticker.C:
			message := t.String()
			m = message
			err := c.WriteMessage(websocket.TextMessage, []byte(message))
			if err != nil {
				log.Println("client write:", err)
				return
			}
			log.Println("============================")
			log.Println("client send:", message)
		case <-interrupt:
			log.Println("interrupt")

			// Cleanly close the connection by sending a close message and then
			// waiting (with timeout) for the server to close the connection.
			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Println("client write close:", err)
				return
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return
		}
	}
}
