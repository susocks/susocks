package main

import (
	"context"
	"crypto/md5"
	"golang.org/x/net/proxy"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)
}

func main() {

	reqUrl := "http://tmp.link/d/5d986c61ce0e9"
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
	req.Header.Add("Referer", "http://tmp.link/f/5d986c61ce0e9")
	req.Header.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3")
	req.Header.Add("Cookie", "_ga=GA1.2.1181804735.1570014602; _gid=GA1.2.477743204.1570270287; _gat_gtag_UA_96864664_3=1")
	req.Header.Add("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/76.0.3809.100 Safari/537.36")
	req.Header.Add("Connection", "keep-alive")
	//req.Header.Add("Accept-Encoding","gzip, deflate")
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

	log.Printf("%x", md5sum)
	log.Print("6be0e7d86e4b2a5c7e2b43ed56b74463")
	log.Printf("%s", data[:256])
	//log.Printf("%x")
}

func DialerAddContext(dialer proxy.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}
}
