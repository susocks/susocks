package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"golang.org/x/net/proxy"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

func main() {
	waitGroup := new(sync.WaitGroup)
	for i := 0; i < 1; i++ {
		waitGroup.Add(1)
		go func() {
			sum := Fetch()
			log.Print(sum)
			waitGroup.Done()
		}()
	}
	waitGroup.Wait()
}

func Fetch() string {
	reqUrl := "https://gdown.baidu.com/data/wisegame/dc976e3ab67c80b0/baidushoujizhushou_16798012.apk"
	u, err := url.Parse("socks5://localhost:1081")
	if err != nil {
		log.Fatal(err)
	}
	dialer, err := proxy.FromURL(u, nil)
	if err != nil {
		log.Fatal(err)
	}
	c := http.Client{
		Transport: &http.Transport{
			DialContext: DialerAddContext(dialer),
		},
	}
	req, err := http.NewRequest("GET", reqUrl, nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Add("Connection", "keep-alive")
	req.Header.Add("Pragma", "no-cache")
	req.Header.Add("Cache-Control", "no-cache")
	req.Header.Add("Upgrade-Insecure-Requests", "1")
	req.Header.Add("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/76.0.3809.100 Safari/537.36")
	req.Header.Add("DNT", "1")
	req.Header.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3")
	req.Header.Add("Accept-Encoding", "gzip, deflate")
	req.Header.Add("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7,zh-TW;q=0.6")
	resp, err := c.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	md5sum := md5.Sum(data)
	//log.Print(resp.Header)
	return fmt.Sprintf("%x", md5sum)
	//log.Print("6be0e7d86e4b2a5c7e2b43ed56b74463")
	//log.Printf("%s", data[:256])
	//log.Printf("%x")
}

func DialerAddContext(dialer proxy.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}
}
