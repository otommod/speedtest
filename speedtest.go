package speedtest

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dchest/uniuri"
	"github.com/zpeters/speedtest/internal/coords"
	"golang.org/x/sync/errgroup"
)

var (
	SpeedtestNetConfigURL  = "http://speedtest.net/speedtest-config.php?x=" + uniuri.New()
	SpeedtestNetServersURL = "http://speedtest.net/speedtest-servers.php?x=" + uniuri.New()
)

type Client struct {
	UserAgent string

	httpClient *http.Client
}

func NewClient(httpClient *http.Client) Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return Client{
		httpClient: httpClient,
	}
}

func (c Client) getResource(url string, xmlData interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-OK status code: %s", resp.Status)
	}
	return xml.NewDecoder(resp.Body).Decode(xmlData)
}

type Config struct {
	Client   ConfigClient `xml:"client"`
	Server   ConfigServer `xml:"server-config"`
	Latency  ConfigTest   `xml:"latency"`
	Download ConfigTest   `xml:"download"`
	Upload   ConfigTest   `xml:"upload"`
}

type ConfigClient struct {
	IP        string  `xml:"ip,attr"`
	ISP       string  `xml:"isp,attr"`
	Country   string  `xml:"country,attr"`
	Latitude  float64 `xml:"lat,attr"`
	Longitude float64 `xml:"lon,attr"`
}

type UnmarshableStringSlice []string

func (ss *UnmarshableStringSlice) UnmarshalText(text []byte) error {
	*ss = strings.Split(string(text), ",")
	return nil
}

type ConfigServer struct {
	IgnoreIDs   UnmarshableStringSlice `xml:"ignoreids,attr"`
	ThreadCount int                    `xml:"threadcount,attr"`
}

type ConfigTest struct {
	TestLengthSeconds float64 `xml:"testlength,attr"`
}

func (c Client) GetSpeedtestConfig(configURL string) (Config, error) {
	var cx struct {
		XMLName xml.Name `xml:"settings"`
		Config
	}

	err := c.getResource(configURL, &cx)
	return cx.Config, err
}

type UnmarshableURL url.URL

func (u *UnmarshableURL) UnmarshalText(text []byte) error {
	return (*url.URL)(u).UnmarshalBinary(text)
}

type Server struct {
	URL         *UnmarshableURL `xml:"url,attr"`
	URL2        *UnmarshableURL `xml:"url2,attr"`
	Host        string          `xml:"host,attr"`
	Latitude    float64         `xml:"lat,attr"`
	Longitude   float64         `xml:"lon,attr"`
	Name        string          `xml:"name,attr"`
	Country     string          `xml:"country,attr"`
	CountryCode string          `xml:"cc,attr"`
	Sponsor     string          `xml:"sponsor,attr"`
	ID          string          `xml:"id,attr"`
	Distance    float64         `xml:"-"`
	Latency     float64         `xml:"-"`
}

func (c Client) GetServers(serverListURL string) ([]Server, error) {
	var sx struct {
		XMLName xml.Name `xml:"settings"`
		Servers []Server `xml:"servers>server"`
	}

	err := c.getResource(serverListURL, &sx)
	return sx.Servers, err
}

func ComputeServersDistanceTo(servers []Server, lat, lon float64) error {
	myPos := coords.DegPos(lat, lon)
	for i, srv := range servers {
		srvPos := coords.DegPos(srv.Latitude, srv.Longitude)
		servers[i].Distance = coords.HsDist(myPos, srvPos)
	}
	return nil
}

type meteredConn struct {
	tx, rx *int32
	net.Conn
}

func (c meteredConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err == nil {
		atomic.AddInt32(c.rx, int32(n))
	}
	return n, err
}

func (c meteredConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil {
		atomic.AddInt32(c.tx, int32(n))
	}
	return n, err
}

type meteredDialer struct {
	tx, rx int32
	*net.Dialer
}

func (d *meteredDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := d.Dialer.DialContext(ctx, network, addr)
	if err != nil {
		return c, err
	}
	return meteredConn{&d.tx, &d.rx, c}, err
}

type Tester struct {
	UserAgent   string
	ThreadCount int
}

func NewTester(conf Config) *Tester {
	return &Tester{
		ThreadCount: conf.Server.ThreadCount,
	}
}

func (c *Tester) dialer() *meteredDialer {
	return &meteredDialer{
		Dialer: &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
}

func (c *Tester) httpClient(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			TLSNextProto:       make(map[string]func(string, *tls.Conn) http.RoundTripper),
			DialContext:        dialContext,
		},
	}
}

func (t *Tester) newRequest(method, url string, contentLength int64, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = contentLength
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", t.UserAgent)
	return req, nil
}

func (t *Tester) do(ctx context.Context, httpClient *http.Client, req *http.Request) error {
	fmt.Println(req.Method, req.URL.String())

	resp, err := httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-OK status code: %s", resp.Status)
	}

	_, err = io.Copy(ioutil.Discard, resp.Body)
	return err
}

type Measurement struct {
	Bytes, Seconds float64
}

func (t Tester) Latency(ctx context.Context, srv Server, numTests uint) (<-chan Measurement, <-chan error) {
	ch := make(chan Measurement)
	errch := make(chan error, 1)

	latencyURL, err := (*url.URL)(srv.URL).Parse("./latency.txt")
	if err != nil {
		errch <- err
		close(ch)
		close(errch)
		return ch, errch
	}

	go func() {
		defer close(ch)
		defer close(errch)

		var wg sync.WaitGroup
		for ; numTests > 0; numTests-- {
			req, err := t.newRequest("GET", latencyURL.String(), 0, nil)
			if err != nil {
				errch <- err
				return
			}

			var start time.Time
			trace := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				// XXX: will this only get called once? what about redirects?
				GotFirstResponseByte: func() {
					defer wg.Done()

					select {
					case <-ctx.Done():
					case ch <- Measurement{
						Seconds: time.Since(start).Seconds()}:
					}
				},
			})

			wg.Add(1)
			start = time.Now()
			err = t.do(trace, http.DefaultClient, req)
			if err != nil {
				errch <- err
				return
			}

			select {
			case <-ctx.Done():
				errch <- ctx.Err()
				return
			default:
			}
		}

		wg.Wait()
	}()

	return ch, errch
}

func (t Tester) Download(ctx context.Context, srv Server, sizes []uint) (<-chan Measurement, <-chan error) {
	g, ctx := errgroup.WithContext(ctx)

	srvURL := (*url.URL)(srv.URL)
	dialer := t.dialer()
	httpClient := t.httpClient(dialer.DialContext)

	urlch := make(chan string)
	g.Go(func() error {
		defer close(urlch)

		for _, size := range sizes {
			downloadURL, err := srvURL.Parse(fmt.Sprintf("random%dx%d.jpg", size, size))
			if err != nil {
				return err
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case urlch <- downloadURL.String():
			}
		}
		return nil
	})

	threads := t.ThreadCount
	if threads < 1 {
		threads = 1
	}
	for i := 0; i < threads; i++ {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()

				case u, valid := <-urlch:
					if !valid {
						return nil
					}

					req, err := t.newRequest("GET", u, 0, nil)
					if err != nil {
						return err
					}

					err = t.do(ctx, httpClient, req)
					if err != nil {
						return err
					}
				}
			}
		})
	}

	ch := make(chan Measurement)
	go func() {
		defer close(ch)

		// TODO: make the reporting interval configurable
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		lastTick := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ch <- Measurement{
					Bytes:   float64(atomic.SwapInt32(&dialer.rx, 0)),
					Seconds: time.Since(lastTick).Seconds(),
				}
				lastTick = time.Now()
			}
		}
	}()

	errch := make(chan error, 1)
	go func() {
		errch <- g.Wait()
		close(errch)
	}()

	return ch, errch
}

func (t Tester) Upload(ctx context.Context, srv Server, sizes []uint) (<-chan Measurement, <-chan error) {
	g, ctx := errgroup.WithContext(ctx)

	srvURL := (*url.URL)(srv.URL).String()
	dialer := t.dialer()
	httpClient := t.httpClient(dialer.DialContext)

	sizech := make(chan int64)
	g.Go(func() error {
		defer close(sizech)

		for _, size := range sizes {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case sizech <- int64(size):
			}
		}
		return nil
	})

	threads := t.ThreadCount
	if threads < 1 {
		threads = 1
	}
	for i := 0; i < threads; i++ {
		g.Go(func() error {
			rng := rand.New(rand.NewSource(1))
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()

				case size, valid := <-sizech:
					if !valid {
						return nil
					}

					body := io.LimitReader(rng, size)
					req, err := t.newRequest("POST", srvURL, size, body)
					if err != nil {
						return err
					}

					err = t.do(ctx, httpClient, req)
					if err != nil {
						return err
					}
				}
			}
		})
	}

	ch := make(chan Measurement)
	ticker := time.NewTicker(time.Second)
	go func() {
		defer ticker.Stop()
		defer close(ch)

		lastTick := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ch <- Measurement{
					Bytes:   float64(atomic.SwapInt32(&dialer.tx, 0)),
					Seconds: time.Since(lastTick).Seconds(),
				}
				lastTick = time.Now()
			}
		}
	}()

	errch := make(chan error, 1)
	go func() {
		defer close(errch)

		err := g.Wait()
		if err != nil {
			errch <- err
		}
	}()

	return ch, errch
}

// ByDistance implements sort.Interface to allow sorting servers by distance
type ByDistance []Server

func (srv ByDistance) Len() int {
	return len(srv)
}

func (srv ByDistance) Less(i, j int) bool {
	return srv[i].Distance < srv[j].Distance
}

func (srv ByDistance) Swap(i, j int) {
	srv[i], srv[j] = srv[j], srv[i]
}

// ByLatency implements sort.Interface to allow sorting servers by latency
type ByLatency []Server

func (srv ByLatency) Len() int {
	return len(srv)
}

func (srv ByLatency) Less(i, j int) bool {
	return srv[i].Latency < srv[j].Latency
}

func (srv ByLatency) Swap(i, j int) {
	srv[i], srv[j] = srv[j], srv[i]
}
