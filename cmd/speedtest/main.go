package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/zpeters/speedtest"
)

// Version placeholder, injected in Makefile
var Version string

func average(ch <-chan speedtest.Measurement) (avg speedtest.Measurement) {
	var ticks float64
	for m := range ch {
		ticks++
		avg.Bytes += m.Bytes
		avg.Seconds += m.Seconds
	}

	avg.Bytes /= ticks
	avg.Seconds /= ticks
	return
}

func listServers() {
	client := speedtest.NewClient(http.DefaultClient)
	client.UserAgent = userAgent

	if *debug {
		log.Println("Getting servers list...")
	}

	allServers, err := client.GetServers(serversURL)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("(%d) found\n", len(allServers))
	for _, srv := range allServers {
		fmt.Printf("%-6s | %s (%s, %s)\n", srv.ID, srv.Sponsor, srv.Name, srv.Country)
	}
}

func runTest() {
	client := speedtest.NewClient(http.DefaultClient)
	client.UserAgent = userAgent

	config, err := client.GetSpeedtestConfig(configURL)
	if err != nil {
		log.Fatalln("Cannot get speedtest config:", err)
	}
	tester := speedtest.NewTester(config)

	// get all possible servers (excluding blacklisted)
	if *debug {
		log.Printf("Getting all servers for our test list")
	}

	var allServers []speedtest.Server
	if *mini == "" {
		allServers, err = client.GetServers(serversURL)
		if err != nil {
			log.Fatal(err)
		}
	}

	if *mini != "" {
		miniURL, err := url.Parse(*mini)
		if err != nil {
			log.Fatal(err)
		}

		miniUploadURL, err := miniURL.Parse("./speedtest/upload.php")
		if err != nil {
			log.Fatal(err)
		}

		miniServer := speedtest.Server{
			URL:     (*speedtest.UnmarshableURL)(miniUploadURL),
			Name:    miniURL.Hostname(),
			Sponsor: "speedtest-mini",
			ID:      "0",
		}
		allServers = []speedtest.Server{miniServer}

	} else if *server != "" {
		var found []speedtest.Server
		for _, srv := range allServers {
			if srv.ID == *server {
				found = append(found, srv)
			}
		}
		allServers = found

		if len(allServers) != 1 {
			log.Fatalln("Cannot find specified server ID", *server)
		}

	} else {
		speedtest.ComputeServersDistanceTo(allServers, config.Client.Latitude, config.Client.Longitude)
		sort.Sort(speedtest.ByDistance(allServers))
	}

	for i := range allServers[:numClosestServers] {
		ch, errch := tester.Latency(context.Background(), allServers[i], numLatencyTests)

		allServers[i].Latency = average(ch).Seconds

		if err = <-errch; err != nil {
			log.Fatal(err)
		}
	}
	sort.Sort(speedtest.ByLatency(allServers[:numClosestServers]))

	testServer := allServers[0]
	fmt.Printf("Server: %s - %s (%s)\n", testServer.ID, testServer.Name, testServer.Sponsor)

	// if ping only then just output latency results and exit nicely...
	if *pingOnly {
		fmt.Printf("Ping (Avg): %3.2f ms\n", 1000*testServer.Latency)

	} else {
		var downspeed, upspeed float64

		if *downloadOnly || !*uploadOnly {
			ch, errch := tester.Download(context.TODO(), testServer, downloadSizes)

			ch2 := make(chan speedtest.Measurement)
			go func() {
				defer close(ch2)
				for m := range ch {
					fmt.Print(".")
					ch2 <- m
				}
			}()

			m := average(ch2)
			downspeed = m.Bytes / m.Seconds

			if err = <-errch; err != nil {
				log.Fatal(err)
			}
		}

		if *uploadOnly || !*downloadOnly {
			ch, errch := tester.Upload(context.TODO(), testServer, uploadSizes)

			ch2 := make(chan speedtest.Measurement)
			go func() {
				defer close(ch2)
				for m := range ch {
					fmt.Print(".")
					ch2 <- m
				}
			}()

			m := average(ch2)
			upspeed = m.Bytes / m.Seconds

			if err = <-errch; err != nil {
				log.Fatal(err)
			}
		}

		fmt.Println()
		fmt.Printf("Ping (Avg): %3.2f ms | Download (Avg): %3.2f Mbps | Upload (Avg): %3.2f Mbps\n", 1000*testServer.Latency, (8*downspeed)/1e6, (8*upspeed)/1e6)
	}
}

const (
	userAgent         = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.21 Safari/537.36"
	numClosestServers = 5
	numLatencyTests   = 3
)

var (
	configURL  = speedtest.SpeedtestNetConfigURL
	serversURL = speedtest.SpeedtestNetServersURL

	downloadSizes = []uint{350, 500, 750, 1000, 1500, 2000, 2500, 3000, 3500, 4000}
	uploadSizes   = []uint{0.25 * 1e6, 0.5 * 1e6, 1.0 * 1e6, 1.5 * 1e6, 2.0 * 1e6}
)

var (
	showVersion = flag.Bool("version", false, "Show version and exit")
	list        = flag.Bool("list", false, "List available servers and quit")

	pingOnly     = flag.Bool("ping", false, "Find the fastest server, show its latency and quit")
	downloadOnly = flag.Bool("download", false, "Only perform download test")
	uploadOnly   = flag.Bool("upload", false, "Only perform upload test")

	server = flag.String("server", "", "Use a specific server")
	mini   = flag.String("mini", "", "URL of speedtest mini server")
	// iface  = flag.String("interface", "", "Source IP address or name of an interface")
	debug = flag.Bool("debug", false, "Turn on debugging")

	blacklist stringSliceFlag
)

type stringSliceFlag []string

func (ss *stringSliceFlag) String() string {
	return fmt.Sprint(*ss)
}

func (ss *stringSliceFlag) Set(value string) error {
	*ss = append(*ss, strings.Split(value, ",")...)
	return nil
}

func main() {
	// flag.Var(&blacklist, "blacklist", "Blacklist a server.  Use this multiple times for mode than one server")

	flag.Parse()

	// 	client := speedtest.NewClient(http.DefaultClient)
	// 	client.UserAgent = userAgent

	// 	config, err := client.GetSpeedtestConfig(configURL)
	// 	if err != nil {
	// 		log.Fatalln("Cannot get speedtest config:", err)
	// 	}
	// 	tester := speedtest.NewTester(config)

	// 	// get all possible servers (excluding blacklisted)
	// 	if *debug {
	// 		log.Printf("Getting all servers for our test list")
	// 	}

	// 	var allServers []speedtest.Server
	// 	if *mini == "" {
	// 		allServers, err = client.GetServers(serversURL)
	// 		if err != nil {
	// 			log.Fatal(err)
	// 		}
	// 	}

	if *showVersion {
		fmt.Println("speedtest", Version)
	} else if *list {
		listServers()
	} else {
		runTest()
	}
}
