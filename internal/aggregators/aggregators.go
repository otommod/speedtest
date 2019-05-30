package aggregators

import (
	"time"

	"github.com/zpeters/speedtest"
)

type Aggregator interface {
	Latency(ch <-chan time.Duration) time.Duration
	Bandwidth(ch <-chan speedtest.Measurement) speedtest.Measurement
}

func Average(ch <-chan speedtest.Measurement) speedtest.Measurement {
	var ticks float64
	var avg speedtest.Measurement
	for m := range ch {
		ticks++
		avg.Bytes += m.Bytes
		avg.Seconds += m.Seconds
	}

	avg.Bytes /= ticks
	avg.Seconds /= ticks
	return avg
}

type avg struct{}

func (avg) Latency(ch <-chan time.Duration) (avg time.Duration) {
	var ticks time.Duration
	for x := range ch {
		ticks++
		avg += x
	}

	avg /= ticks
	return
}

func (avg) Bandwidth(ch <-chan speedtest.Measurement) (avg speedtest.Measurement) {
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

type best struct{}

func (best) Latency(ch <-chan time.Duration) (best time.Duration) {
	for m := range ch {
		if m < best || best == 0 {
			best = m
		}
	}
	return
}

func (best) Bandwidth(ch <-chan speedtest.Measurement) (best speedtest.Measurement) {
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

var (
	Best = best{}
)
