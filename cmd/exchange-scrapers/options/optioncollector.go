package main

import (
	"flag"
	"sync"
	"time"

	"github.com/diadata-org/diadata/pkg/dia"
	options "github.com/diadata-org/diadata/pkg/dia/scraper/option-scrapers"
	models "github.com/diadata-org/diadata/pkg/model"
	"github.com/sirupsen/logrus"
)

var log *logrus.Logger

func init() {
	log = logrus.New()
}

const (
	watchdogDelay = 60 * 60
)

func handleorderBook(datastore *models.DB, c chan *dia.OptionOrderbookDatum, wg *sync.WaitGroup) {
	lastTradeTime := time.Now()
	t := time.NewTicker(time.Duration(watchdogDelay) * time.Second)
	for {
		select {
		case <-t.C:
			duration := time.Since(lastTradeTime)
			if duration > time.Duration(watchdogDelay)*time.Second {
				log.Error(duration)
				panic("frozen? ")
			}
		case t, ok := <-c:
			if !ok {
				wg.Done()
				log.Error("handleTrades")
				return
			}
			lastTradeTime = time.Now()
			err := datastore.SaveOptionOrderbookDatumInflux(*t)
			if err != nil {
				log.Error(err)
			}

		}
	}
}

var (
	exchange = flag.String("exchange", "", "which exchange")

//	onePairPerSymbol = flag.Bool("onePairPerSymbol", false, "one Pair max Per Symbol ?")
)

func init() {
	flag.Parse()
	if *exchange == "" {
		flag.Usage()
		for {
			time.Sleep(24 * time.Hour)
		}
		// log.Fatal("exchange is required")
	}
}

// main manages all PairScrapers and handles incoming trade information
func main() {

	ds, err := models.NewDataStore()
	if err != nil {
		log.Errorln("NewDataStore:", err)
	}

	configApi, err := dia.GetConfig(*exchange)
	if err != nil {
		log.Warning("no config for exchange's api ", err)
	}
	es := options.New(*exchange, configApi.ApiKey, configApi.SecretKey)

	es.FetchInstruments()
	es.Scrape()

	wg := sync.WaitGroup{}
	wg.Add(1)

	go handleorderBook(ds, es.Channel(), &wg)
	wg.Wait()
}
