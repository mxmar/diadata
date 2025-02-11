package scrapers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/balancer/balancerfactory"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/balancer/balancerpool"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/balancer/balancertoken"

	"github.com/diadata-org/diadata/pkg/dia/helpers/ethhelper"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/event"

	"github.com/diadata-org/diadata/pkg/dia"
	"github.com/diadata-org/diadata/pkg/dia/helpers"
)

const (
	BalancerApiDelay       = 20
	BalancerBatchDelay     = 60 * 1
	lookBackBlocksSwaps    = 6 * 60 * 24 * 10
	startBlockPoolCreation = uint64(9600000)
	factoryContract        = "0x9424B1412450D0f8Fc2255FAf6046b98213B76Bd"
	balancerRestDial       = "http://159.69.120.42:8545/"
	balancerWsDial         = "ws://159.69.120.42:8546/"
)

type BalancerSwap struct {
	SellToken  string
	BuyToken   string
	SellVolume float64
	BuyVolume  float64
	ID         string
	Timestamp  int64
}

type BalancerToken struct {
	Symbol   string
	Decimals uint8
}

type BalancerScraper struct {
	exchangeName string

	// channels to signal events
	run          bool
	initDone     chan nothing
	shutdown     chan nothing
	shutdownDone chan nothing

	errorLock sync.RWMutex
	error     error
	closed    bool

	balancerTokensMap map[string]dia.Asset
	pairScrapers      map[string]*BalancerPairScraper
	productPairIds    map[string]int
	chanTrades        chan *dia.Trade

	WsClient    *ethclient.Client
	RestClient  *ethclient.Client
	resubscribe chan string
	pools       map[string]struct{}
}

func NewBalancerScraper(exchange dia.Exchange, scrape bool) *BalancerScraper {
	scraper := &BalancerScraper{
		exchangeName:      exchange.Name,
		initDone:          make(chan nothing),
		shutdown:          make(chan nothing),
		shutdownDone:      make(chan nothing),
		productPairIds:    make(map[string]int),
		pairScrapers:      make(map[string]*BalancerPairScraper),
		chanTrades:        make(chan *dia.Trade),
		balancerTokensMap: make(map[string]dia.Asset),
		resubscribe:       make(chan string),
		pools:             make(map[string]struct{}),
	}

	wsClient, err := ethclient.Dial(balancerWsDial)
	if err != nil {
		log.Fatal(err)
	}
	scraper.WsClient = wsClient
	restClient, err := ethclient.Dial(balancerRestDial)
	if err != nil {
		log.Fatal(err)
	}
	scraper.RestClient = restClient

	if scrape {
		go scraper.mainLoop()
	}
	return scraper
}

func (scraper *BalancerScraper) mainLoop() {

	time.Sleep(5 * time.Second)

	scraper.run = true

	scraper.balancerTokensMap, _ = scraper.getAllTokensMap()

	pools, err := scraper.getAllLogNewPool()
	if err != nil {
		log.Error(err)
	}
	for pools.Next() {
		scraper.pools[pools.Event.Pool.Hex()] = struct{}{}
	}
	scraper.performSubscriptions()

	go func() {
		for scraper.run {
			pool := <-scraper.resubscribe

			if scraper.run {
				if pool == "NEW_POOLS" {
					log.Info("resubscribe to new pools")
					err = scraper.subscribeToNewPools()
					if err != nil {
						log.Error(err)
					}
				} else {
					log.Info("resubscribe to pool: " + pool)
					err = scraper.subscribeToNewSwaps(pool)
					if err != nil {
						log.Error(err)
					}
				}
			}
		}
	}()

	if scraper.run {
		if len(scraper.pairScrapers) == 0 {
			scraper.error = errors.New("no pairs to scrape provided")
			log.Error(scraper.error.Error())
		}
	}

	time.Sleep(10 * time.Second)

	if scraper.error == nil {
		scraper.error = errors.New("main loop terminated by Close()")
	}
	scraper.cleanup(nil)

}

func (scraper *BalancerScraper) performSubscriptions() {
	for pool := range scraper.pools {
		err := scraper.subscribeToNewSwaps(pool)
		if err != nil {
			log.Error(err)
		}
	}

	err := scraper.subscribeToNewPools()
	if err != nil {
		log.Error(err)
	}
}

func (scraper *BalancerScraper) subscribeToNewPools() error {
	sinkPool, subPool, err := scraper.getNewPoolLogChannel()
	if err != nil {
		log.Error(err)
	}
	go func() {
		fmt.Println("subscribed to NewPools")
		defer fmt.Println("Unsubscribed to NewPools")
		defer subPool.Unsubscribe()
		subscribed := true

		for scraper.run && subscribed {

			select {
			case err = <-subPool.Err():
				if err != nil {
					log.Error(err)
				}
				subscribed = false
				if scraper.run {
					scraper.resubscribe <- "NEW_POOLS"
				}
			case vLog := <-sinkPool:
				if _, ok := scraper.pools[vLog.Pool.Hex()]; !ok {
					scraper.pools[vLog.Pool.Hex()] = struct{}{}
					err = scraper.subscribeToNewSwaps(vLog.Pool.Hex())
					if err != nil {
						log.Error(err)
					}
				}
			}
		}
	}()

	return err
}

func (scraper *BalancerScraper) subscribeToNewSwaps(poolToSub string) (err error) {
	sink, sub := scraper.getLogSwapsChannel(common.HexToAddress(poolToSub))

	go func() {
		fmt.Println("subscribed to pool: " + poolToSub)
		defer fmt.Println("Unsubscribed to pool: " + poolToSub)
		defer sub.Unsubscribe()
		subscribed := true
		for scraper.run && subscribed {

			select {
			case err = <-sub.Err():
				if err != nil {
					log.Error(err)
				}
				subscribed = false
				if scraper.run {
					scraper.resubscribe <- poolToSub
				}
			case vLog := <-sink:

				decimalsIn := int(scraper.balancerTokensMap[vLog.TokenIn.Hex()].Decimals)
				decimalsOut := int(scraper.balancerTokensMap[vLog.TokenOut.Hex()].Decimals)
				amountIn, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(vLog.TokenAmountIn), new(big.Float).SetFloat64(math.Pow10(decimalsIn))).Float64()
				amountOut, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(vLog.TokenAmountOut), new(big.Float).SetFloat64(math.Pow10(decimalsOut))).Float64()
				swap := BalancerSwap{
					SellToken:  scraper.balancerTokensMap[vLog.TokenIn.Hex()].Symbol,
					BuyToken:   scraper.balancerTokensMap[vLog.TokenOut.Hex()].Symbol,
					SellVolume: amountIn,
					BuyVolume:  amountOut,
					ID:         vLog.Raw.TxHash.String() + "-" + fmt.Sprint(vLog.Raw.Index),
					Timestamp:  time.Now().Unix(),
				}
				swap.normalizeETH()
				pair := swap.BuyToken + "-" + swap.SellToken
				pairScraper, ok := scraper.pairScrapers[pair]
				if !ok {
					err = errors.New("pair does not have a corresponding pair scraper")
					return
				}

				// Get trading data from swap in "classic" format
				_, volume, price := getSwapDataBalancer(swap)

				trade := &dia.Trade{
					Symbol:         pairScraper.pair.Symbol,
					Pair:           pair,
					Price:          price,
					Volume:         volume,
					Time:           time.Unix(swap.Timestamp, 0),
					ForeignTradeID: swap.ID,
					Source:         scraper.exchangeName,
					BaseToken:      scraper.balancerTokensMap[vLog.TokenIn.Hex()],
					QuoteToken:     scraper.balancerTokensMap[vLog.TokenOut.Hex()],
					VerifiedPair:   true,
				}
				pairScraper.parent.chanTrades <- trade
				fmt.Println("got trade: ", trade)

			}
		}
	}()
	return
}

// getSwapData returns the foreign name, volume and price of a swap
func getSwapDataBalancer(s BalancerSwap) (foreignName string, volume float64, price float64) {
	volume = s.BuyVolume
	price = s.SellVolume / s.BuyVolume
	foreignName = s.BuyToken + "-" + s.SellToken
	return
}

func (s *BalancerScraper) NormalizePair(pair dia.ExchangePair) (dia.ExchangePair, error) {
	return dia.ExchangePair{}, nil
}

func (scraper *BalancerScraper) getAllLogNewPool() (*balancerfactory.BalancerfactoryLOGNEWPOOLIterator, error) {

	var pairFiltererContract *balancerfactory.BalancerfactoryFilterer
	pairFiltererContract, err := balancerfactory.NewBalancerfactoryFilterer(common.HexToAddress(factoryContract), scraper.RestClient)
	if err != nil {
		log.Fatal(err)
	}
	// header, err := scraper.RestClient.HeaderByNumber(context.Background(), nil)
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// startblock := header.Number.Uint64() - uint64(BalancerLookBackBlocks)

	itr, _ := pairFiltererContract.FilterLOGNEWPOOL(&bind.FilterOpts{Start: startBlockPoolCreation}, []common.Address{}, []common.Address{})
	if err != nil {
		log.Error("error in getAllLogNewPool ", err)
	}
	return itr, err
}

func (scraper *BalancerScraper) getAllTokenAddress() (map[string]struct{}, error) {
	it, err := scraper.getAllLogNewPool()
	if err != nil {
		log.Error(err)
	}

	tokenSet := make(map[string]struct{})
	for it.Next() {
		var poolCaller *balancerpool.BalancerpoolCaller
		var tokens []common.Address
		poolCaller, err = balancerpool.NewBalancerpoolCaller(it.Event.Pool, scraper.RestClient)
		if err != nil {
			log.Error(err)
		}

		tokens, err = poolCaller.GetCurrentTokens(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}

		for _, token := range tokens {

			if _, ok := tokenSet[token.Hex()]; !ok {
				tokenSet[token.Hex()] = struct{}{}
			}
		}
	}

	return tokenSet, err
}

func (scraper *BalancerScraper) getAllTokensMap() (map[string]dia.Asset, error) {
	tokenAddressSet, err := scraper.getAllTokenAddress()
	if err != nil {
		log.Error(err)
	}

	tokenMap := make(map[string]dia.Asset)

	for token := range tokenAddressSet {
		var tokenCaller *balancertoken.BalancertokenCaller
		var symbol string
		var name string
		var decimals *big.Int
		tokenCaller, err = balancertoken.NewBalancertokenCaller(common.HexToAddress(token), scraper.RestClient)
		if err != nil {
			log.Error(err)
		}
		symbol, err = tokenCaller.Symbol(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		name, err = tokenCaller.Name(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		if helpers.SymbolIsBlackListed(symbol) {
			continue
		}
		decimals, err = tokenCaller.Decimals(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		if symbol != "" {
			tokenMap[token] = dia.Asset{
				Symbol:     symbol,
				Name:       name,
				Decimals:   uint8(decimals.Uint64()),
				Address:    common.HexToAddress(token).Hex(),
				Blockchain: dia.ETHEREUM,
			}
		}
	}
	return tokenMap, err
}

// FetchAvailablePairs get pairs by getting all the LOGNEWPOOL contract events, and
// calling the method getCurrentTokens from each pool contract
func (scraper *BalancerScraper) FetchAvailablePairs() (pairs []dia.ExchangePair, err error) {
	it, err := scraper.getAllLogNewPool()
	if err != nil {
		log.Error(err)
	}
	poolCount := 0
	for it.Next() {
		var pair dia.ExchangePair
		poolCaller, err := balancerpool.NewBalancerpoolCaller(it.Event.Pool, scraper.RestClient)
		if err != nil {
			log.Error(err)
		}
		tokens, err := poolCaller.GetCurrentTokens(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		if len(tokens) < 2 {
			continue
		}
		for i := 0; i < len(tokens); i++ {
			j := i + 1
			for j < len(tokens) {
				asset0, err := ethhelper.ETHAddressToAsset(tokens[i], scraper.RestClient, dia.ETHEREUM)
				if err != nil {
					continue
				}
				asset1, err := ethhelper.ETHAddressToAsset(tokens[j], scraper.RestClient, dia.ETHEREUM)
				if err != nil {
					continue
				}
				pair.UnderlyingPair.QuoteToken = asset0
				pair.UnderlyingPair.BaseToken = asset1
				pair.ForeignName = asset0.Symbol + "-" + asset1.Symbol
				pair.Verified = true
				pair.Exchange = "Balancer"
				pair.Symbol = asset0.Symbol
				pairs = append(pairs, pair)
				j++
			}
		}
		log.Info("got pool number: ", poolCount)
		poolCount++
	}

	return

}

// func (scraper *BalancerScraper) getLogSwapsChannelFilter(address string) (chan *pool.BalancerpoolLOGSWAP, error) {
// 	sink := make(chan *pool.BalancerpoolLOGSWAP)
// 	var pairFiltererContract *pool.BalancerpoolFilterer
// 	pairFiltererContract, _ = pool.NewBalancerpoolFilterer(common.HexToAddress(address), scraper.RestClient)

// 	header, err := scraper.RestClient.HeaderByNumber(context.Background(), nil)
// 	if err != nil {
// 		log.Fatal(err)
// 	}
// 	startblock := header.Number.Uint64() - uint64(5250)

// 	itr, _ := pairFiltererContract.FilterLOGSWAP(&bind.FilterOpts{Start: startblock}, []common.Address{}, []common.Address{}, []common.Address{})
// 	scraper.balancerTokensMap, _ = scraper.getAllTokensMap()
// 	for itr.Next() {
// 		// vLog := itr.Event
// 	}
// 	return sink, nil
// }

func (scraper *BalancerScraper) getLogSwapsChannel(poolAddress common.Address) (chan *balancerpool.BalancerpoolLOGSWAP, event.Subscription) {
	sink := make(chan *balancerpool.BalancerpoolLOGSWAP)
	var pairFiltererContract *balancerpool.BalancerpoolFilterer
	pairFiltererContract, err := balancerpool.NewBalancerpoolFilterer(poolAddress, scraper.WsClient)
	if err != nil {
		log.Fatal(err)
	}

	header, err := scraper.RestClient.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}
	startblock := header.Number.Uint64() - uint64(5250)

	sub, _ := pairFiltererContract.WatchLOGSWAP(&bind.WatchOpts{Start: &startblock}, sink, nil, nil, nil)
	if err != nil {
		log.Error("error in get swaps channel: ", err)
	}

	return sink, sub
}

func (scraper *BalancerScraper) getNewPoolLogChannel() (chan *balancerfactory.BalancerfactoryLOGNEWPOOL, event.Subscription, error) {
	sink := make(chan *balancerfactory.BalancerfactoryLOGNEWPOOL)
	var factoryFiltererContract *balancerfactory.BalancerfactoryFilterer
	factoryFiltererContract, err := balancerfactory.NewBalancerfactoryFilterer(common.HexToAddress(factoryContract), scraper.WsClient)
	if err != nil {
		log.Fatal(err)
	}

	header, err := scraper.RestClient.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}
	startblock := header.Number.Uint64() - uint64(lookBackBlocksSwaps)

	sub, _ := factoryFiltererContract.WatchLOGNEWPOOL(&bind.WatchOpts{Start: &startblock}, sink, nil, nil)
	if err != nil {
		log.Error("error in get pools channel: ", err)
		return sink, sub, err
	}

	return sink, sub, nil
}

func (bs *BalancerSwap) normalizeETH() {
	if bs.SellToken == "WETH" {
		bs.SellToken = "ETH"
	}
	if bs.BuyToken == "WETH" {
		bs.BuyToken = "ETH"
	}

}

func (scraper *BalancerScraper) ScrapePair(pair dia.ExchangePair) (PairScraper, error) {
	scraper.errorLock.RLock()
	defer scraper.errorLock.RUnlock()

	if scraper.error != nil {
		return nil, scraper.error
	}

	if scraper.closed {
		return nil, errors.New("BalancerScraper is closed")
	}

	pairScraper := &BalancerPairScraper{
		parent: scraper,
		pair:   pair,
	}

	scraper.pairScrapers[pair.ForeignName] = pairScraper

	return pairScraper, nil
}

func (scraper *BalancerScraper) FillSymbolData(symbol string) (dia.Asset, error) {
	return dia.Asset{}, nil
}

func (scraper *BalancerScraper) cleanup(err error) {
	scraper.errorLock.Lock()
	defer scraper.errorLock.Unlock()
	if err != nil {
		scraper.error = err
	}
	scraper.closed = true
	close(scraper.shutdownDone)
}

func (scraper *BalancerScraper) Close() error {
	// close the pair scraper channels
	scraper.run = false
	for _, pairScraper := range scraper.pairScrapers {
		pairScraper.closed = true
	}
	scraper.WsClient.Close()
	scraper.RestClient.Close()

	close(scraper.shutdown)
	<-scraper.shutdownDone
	return nil
}

type BalancerPairScraper struct {
	parent *BalancerScraper
	pair   dia.ExchangePair
	closed bool
}

func (pairScraper *BalancerPairScraper) Pair() dia.ExchangePair {
	return pairScraper.pair
}

func (scraper *BalancerScraper) Channel() chan *dia.Trade {
	return scraper.chanTrades
}

func (pairScraper *BalancerPairScraper) Error() error {
	s := pairScraper.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

func (pairScraper *BalancerPairScraper) Close() error {
	pairScraper.parent.errorLock.RLock()
	defer pairScraper.parent.errorLock.RUnlock()
	pairScraper.closed = true
	return nil
}
