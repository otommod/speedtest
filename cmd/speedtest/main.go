/*
speedtest is an unofficial commandline interface to speedtest.net

Version 1.0 was designed as an "app only".  Version 2.0 will make a cleaner split between libraries and interface
*/

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"

	"github.com/zpeters/speedtest"

	"github.com/urfave/cli"
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

func bestLatency(ch <-chan speedtest.Measurement) (best speedtest.Measurement) {
	for m := range ch {
		if m.Seconds < best.Seconds || best.Seconds == 0 {
			best = m
		}
	}
	return
}

func bestBandwidth(ch <-chan speedtest.Measurement) (best speedtest.Measurement) {
	var bestSpeed float64
	for m := range ch {
		var speed float64
		if m.Seconds != 0 {
			speed = m.Bytes / m.Seconds
		}
		if speed > bestSpeed || bestSpeed == 0 {
			bestSpeed = speed
			best = m
		}
	}
	return
}

func listServers(app App) {
	client := speedtest.NewClient(http.DefaultClient)
	client.UserAgent = app.UserAgent

	if app.Debug {
		fmt.Println("Getting servers list...")
	}

	allServers, err := client.GetServers(app.ServersURL)
	if err != nil {
		log.Fatal(err)
	}

	if app.Debug {
		fmt.Printf("(%d) found\n", len(allServers))
	}

	for _, srv := range allServers {
		fmt.Printf("%-6s | %s (%s, %s)\n", srv.ID, srv.Sponsor, srv.Name, srv.Country)
	}
}

func runTest(c *cli.Context, app App) {
	client := speedtest.NewClient(http.DefaultClient)
	client.UserAgent = app.UserAgent

	config, err := client.GetSpeedtestConfig(app.ConfigURL)
	if err != nil {
		log.Fatalln("Cannot get speedtest config:", err)
	}
	tester := speedtest.NewTester(config)

	// get all possible servers (excluding blacklisted)
	if app.Debug {
		log.Printf("Getting all servers for our test list")
	}

	var allServers []speedtest.Server
	if c.String("mini") == "" {
		allServers, err = client.GetServers(app.ServersURL)
		if err != nil {
			log.Fatal(err)
		}
	}

	if c.String("mini") != "" {
		miniURL, err := url.Parse(c.String("mini"))
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

	} else if c.String("server") != "" {
		var found []speedtest.Server
		for _, srv := range allServers {
			if srv.ID == c.String("server") {
				found = append(found, srv)
			}
		}
		allServers = found

		if len(allServers) != 1 {
			log.Fatalln("Cannot find specified server ID", c.String("server"))
		}

	} else {
		speedtest.ComputeServersDistanceTo(allServers, config.Client.Latitude, config.Client.Longitude)
		sort.Sort(speedtest.ByDistance(allServers))
	}

	for i := range allServers[:app.NumClosest] {
		ch, errch := tester.Latency(context.Background(), allServers[i], app.NumLatencyTests)

		if c.String("algo") == "max" {
			allServers[i].Latency = bestLatency(ch).Seconds
		} else {
			allServers[i].Latency = average(ch).Seconds
		}

		if err = <-errch; err != nil {
			log.Fatal(err)
		}
	}
	sort.Sort(speedtest.ByLatency(allServers[:app.NumClosest]))

	testServer := allServers[0]
	fmt.Printf("Server: %s - %s (%s)\n", testServer.ID, testServer.Name, testServer.Sponsor)

	// if ping only then just output latency results and exit nicely...
	if c.Bool("ping") {
		if c.String("algo") == "max" {
			fmt.Printf("Ping (Lowest): %3.2f ms\n", 1000*testServer.Latency)
		} else {
			fmt.Printf("Ping (Avg): %3.2f ms\n", 1000*testServer.Latency)
		}

	} else {
		var downspeed, upspeed float64

		if c.Bool("downloadonly") || !c.Bool("uploadonly") {
			ch, errch := tester.Download(context.TODO(), testServer, app.DLSizes)

			ch2 := make(chan speedtest.Measurement)
			go func() {
				defer close(ch2)
				for m := range ch {
					fmt.Print(".")
					ch2 <- m
				}
			}()

			if c.String("algo") == "max" {
				m := bestBandwidth(ch2)
				downspeed = m.Bytes / m.Seconds
			} else {
				m := average(ch2)
				downspeed = m.Bytes / m.Seconds
			}

			if err = <-errch; err != nil {
				log.Fatal(err)
			}
		}

		if c.Bool("uploadonly") || !c.Bool("downloadonly") {
			ch, errch := tester.Upload(context.TODO(), testServer, app.ULSizes)

			ch2 := make(chan speedtest.Measurement)
			go func() {
				defer close(ch2)
				for m := range ch {
					fmt.Print(".")
					ch2 <- m
				}
			}()

			if c.String("algo") == "max" {
				m := bestBandwidth(ch2)
				upspeed = m.Bytes / m.Seconds
			} else {
				m := average(ch2)
				upspeed = m.Bytes / m.Seconds
			}

			if err = <-errch; err != nil {
				log.Fatal(err)
			}
		}

		var format string
		if c.String("algo") == "max" {
			format = "Ping (Lowest): %3.2f ms | Download (Max): %3.2f Mbps | Upload (Max): %3.2f Mbps\n"
		} else {
			format = "Ping (Avg): %3.2f ms | Download (Avg): %3.2f Mbps | Upload (Avg): %3.2f Mbps\n"
		}

		fmt.Println()
		fmt.Printf(format, 1000*testServer.Latency, (8*downspeed)/1e6, (8*upspeed)/1e6)
	}
}

const (
	defaultUserAgent       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.21 Safari/537.36"
	defaultNumClosest      = 5
	defaultNumLatencyTests = 3
)

var (
	defaultDLSizes = []uint{350, 500, 750, 1000, 1500, 2000, 2500, 3000, 3500, 4000}
	defaultULSizes = []uint{0.25 * 1e6, 0.5 * 1e6, 1.0 * 1e6, 1.5 * 1e6, 2.0 * 1e6}
)

type App struct {
	ConfigURL       string
	ServersURL      string
	AlgoType        string
	NumClosest      uint
	NumLatencyTests uint
	Interface       string
	Blacklist       []string
	UserAgent       string
	DLSizes         []uint
	ULSizes         []uint
	Debug           bool
	Quiet           bool
}

func main() {
	// set logging to stdout for global logger
	log.SetOutput(os.Stdout)

	// setting up cli settings
	app := cli.NewApp()
	app.Name = "speedtest"
	app.Usage = "Unofficial command line interface to speedtest.net (https://github.com/zpeters/speedtest)"
	app.Author = "Zach Peters - zpeters@gmail.com - github.com/zpeters"
	app.Version = Version

	// setup cli flags
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "debug, d",
			Usage: "Turn on debugging",
		},
		cli.BoolFlag{
			Name:  "quiet, q",
			Usage: "Quiet mode",
		},
		cli.BoolFlag{
			Name:  "list, l",
			Usage: "List available servers",
		},
		cli.StringFlag{
			Name:  "algo, a",
			Usage: "Specify the measurement method to use ('max', 'avg')",
		},
		cli.BoolFlag{
			Name:  "ping, p",
			Usage: "Ping only mode",
		},
		cli.BoolFlag{
			Name:  "downloadonly, do",
			Usage: "Only perform download test",
		},
		cli.BoolFlag{
			Name:  "uploadonly, uo",
			Usage: "Only perform upload test",
		},
		cli.StringFlag{
			Name:  "server, s",
			Usage: "Use a specific server",
		},
		cli.StringSliceFlag{
			Name:  "blacklist, b",
			Usage: "Blacklist a server.  Use this multiple times for more than one server",
		},
		cli.StringFlag{
			Name:  "mini, m",
			Usage: "URL of speedtest mini server",
		},
		cli.StringFlag{
			Name:  "useragent, ua",
			Usage: "Specify a useragent string",
		},
		cli.UintFlag{
			Name:  "numclosest, nc",
			Value: defaultNumClosest,
			Usage: "Number of 'closest' servers to find",
		},
		cli.UintFlag{
			Name:  "numlatency, nl",
			Value: defaultNumLatencyTests,
			Usage: "Number of latency tests to perform",
		},
		cli.StringFlag{
			Name:  "interface, I",
			Usage: "Source IP address or name of an interface",
		},
	}

	// toggle our switches and setup variables
	app.Action = func(c *cli.Context) {
		switch c.String("algo") {
		case "", "max", "avg":
		default:
			fmt.Println("** Invalid algorithm:", c.String("algo"))
			os.Exit(1)
		}

		stClient := App{
			ConfigURL:       speedtest.SpeedtestNetConfigURL,
			ServersURL:      speedtest.SpeedtestNetServersURL,
			AlgoType:        c.String("algotype"),
			NumClosest:      c.Uint("numclosest"),
			NumLatencyTests: c.Uint("numlatency"),
			Interface:       c.String("interface"),
			Blacklist:       c.StringSlice("blacklist"),
			UserAgent:       defaultUserAgent,
			DLSizes:         defaultDLSizes,
			ULSizes:         defaultULSizes,
			Debug:           c.Bool("debug"),
			Quiet:           c.Bool("quiet"),
		}

		if c.Bool("list") {
			listServers(stClient)
		} else {
			runTest(c, stClient)
		}

		// exit nicely
		os.Exit(0)
	}

	// run the app
	app.Run(os.Args)
}
