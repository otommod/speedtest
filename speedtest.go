package speedtest

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"github.com/dchest/uniuri"
	"github.com/zpeters/speedtest/internal/coords"
)

var (
	SpeedtestNetConfigURL      = "http://speedtest.net/speedtest-config.php?x=" + uniuri.New()
	SpeedtestNetServersListURL = "http://speedtest.net/speedtest-servers-static.php?x=" + uniuri.New()
)

type Server struct {
	XMLName  xml.Name `xml:"server"`
	URL      string   `xml:"url,attr"`
	Lat      float64  `xml:"lat,attr"`
	Lon      float64  `xml:"lon,attr"`
	Name     string   `xml:"name,attr"`
	Country  string   `xml:"country,attr"`
	CC       string   `xml:"cc,attr"`
	Sponsor  string   `xml:"sponsor,attr"`
	ID       string   `xml:"id,attr"`
	Distance float64  `xml:"-"`
	Latency  float64  `xml:"-"`
}

// Config struct holds our config (users current ip, lat, lon and isp)
type Config struct {
	IP  string  `xml:"ip,attr"`
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
}

type Conf struct {
	HTTPClient *http.Client
	UserAgent  string
}

func getResource(url string, c Conf, xmlData interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-OK status code: %s", resp.Status)
	}

	return xml.NewDecoder(resp.Body).Decode(xmlData)
}

func GetClientConfig(configURL string, c Conf) (Config, error) {
	var cx struct {
		XMLName xml.Name `xml:"settings"`
		Client  Config   `xml:"client"`
	}

	err := getResource(configURL, c, &cx)
	return cx.Client, err
}

func GetServers(serverListURL string, c Conf) ([]Server, error) {
	var sx struct {
		XMLName xml.Name `xml:"settings"`
		Servers []Server `xml:"servers>server"`
	}

	err := getResource(serverListURL, c, &sx)
	return sx.Servers, err
}

func ComputeServersDistanceTo(servers []Server, lat, lon float64) error {
	myPos := coords.DegPos(lat, lon)
	for i, srv := range servers {
		srvPos := coords.DegPos(srv.Lat, srv.Lon)
		servers[i].Distance = coords.HsDist(myPos, srvPos)
	}
	return nil
}

type Measurement struct {
	Bytes, Seconds float64
}

type Tester struct {
	Conf Conf
}

func (t Tester) downloadOne(ctx context.Context, url string) (m Measurement, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", t.Conf.UserAgent)

	start := time.Now()
	resp, err := t.Conf.HTTPClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != 200 {
		err = fmt.Errorf("non-OK status code: %s", resp.Status)
		return
	}

	bodyLen, err := io.Copy(ioutil.Discard, resp.Body)
	return Measurement{float64(bodyLen), elapsed.Seconds()}, err
}

func (t Tester) downloadMany(ctx context.Context, urls ...string) (<-chan Measurement, <-chan error) {
	ch := make(chan Measurement)
	errch := make(chan error, 1)
	urlch := make(chan string)

	go func() {
		defer close(urlch)

		for _, u := range urls {
			select {
			case <-ctx.Done():
				return
			case urlch <- u:
			}
		}
	}()

	go func() {
		defer close(ch)
		defer close(errch)

		for u := range urlch {
			m, err := t.downloadOne(ctx, u)
			if err != nil {
				errch <- err
				return
			}

			select {
			case <-ctx.Done():
				return
			case ch <- m:
			}
		}
	}()

	return ch, errch
}

func (t Tester) Latency(ctx context.Context, srv Server, numTests uint) (<-chan Measurement, <-chan error) {
	dummych := make(chan Measurement)
	errch := make(chan error, 1)
	defer close(dummych)
	defer close(errch)

	u, err := url.Parse(srv.URL)
	if err != nil {
		errch <- err
		return dummych, errch
	}

	latencyURL, err := u.Parse("./latency.txt")
	if err != nil {
		errch <- err
		return dummych, errch
	}

	var urls []string
	for ; numTests > 0; numTests-- {
		urls = append(urls, latencyURL.String())
	}

	return t.downloadMany(ctx, urls...)
}

func (t Tester) Download(ctx context.Context, srv Server, DLSizes []uint) (<-chan Measurement, <-chan error) {
	dummych := make(chan Measurement)
	errch := make(chan error, 1)
	defer close(dummych)
	defer close(errch)

	u, err := url.Parse(srv.URL)
	if err != nil {
		errch <- err
		return dummych, errch
	}

	var urls []string
	for _, size := range DLSizes {
		downloadURL, err := u.Parse(fmt.Sprintf("random%dx%d.jpg", size, size))
		if err != nil {
			errch <- err
			return dummych, errch
		}
		urls = append(urls, downloadURL.String())
	}

	return t.downloadMany(ctx, urls...)
}

func (t Tester) uploadOne(ctx context.Context, url string, contentType string, contentLength int64, data io.Reader) (m Measurement, err error) {
	req, err := http.NewRequest("POST", url, data)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", t.Conf.UserAgent)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", contentLength))

	start := time.Now()
	resp, err := t.Conf.HTTPClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != 200 {
		err = fmt.Errorf("non-OK status code: %s", resp.Status)
		return
	}

	_, err = io.Copy(ioutil.Discard, resp.Body)
	return Measurement{float64(contentLength), elapsed.Seconds()}, nil
}

func (t Tester) Upload(ctx context.Context, srv Server, ULSizes []uint) (<-chan Measurement, <-chan error) {
	ch := make(chan Measurement)
	errch := make(chan error, 1)
	sizech := make(chan int64)
	rng := rand.New(rand.NewSource(1))

	go func() {
		defer close(sizech)

		for _, size := range ULSizes {
			select {
			case <-ctx.Done():
				return
			case sizech <- int64(size):
			}
		}
	}()

	go func() {
		defer close(ch)
		defer close(errch)

		for size := range sizech {
			m, err := t.uploadOne(ctx, srv.URL, "application/octet-stream", size, io.LimitReader(rng, size))
			if err != nil {
				errch <- err
				return
			}

			select {
			case <-ctx.Done():
				return
			case ch <- m:
			}
		}
	}()

	return ch, errch
}

// ByDistance implements sort.Interface to allow sorting servers by distance
type ByDistance []Server

func (server ByDistance) Len() int {
	return len(server)
}

func (server ByDistance) Less(i, j int) bool {
	return server[i].Distance < server[j].Distance
}

func (server ByDistance) Swap(i, j int) {
	server[i], server[j] = server[j], server[i]
}

// ByLatency implements sort.Interface to allow sorting servers by latency
type ByLatency []Server

func (server ByLatency) Len() int {
	return len(server)
}

func (server ByLatency) Less(i, j int) bool {
	return server[i].Latency < server[j].Latency
}

func (server ByLatency) Swap(i, j int) {
	server[i], server[j] = server[j], server[i]
}
