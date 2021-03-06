package huobi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	. "github.com/nntaoli-project/GoEx"
	"io/ioutil"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"regexp"
)

var HBPOINT = NewCurrency("HBPOINT", "")
var onceWsConn sync.Once

var _INERNAL_KLINE_PERIOD_CONVERTER = map[int]string{
	KLINE_PERIOD_1MIN:   "1min",
	KLINE_PERIOD_5MIN:   "5min",
	KLINE_PERIOD_15MIN:  "15min",
	KLINE_PERIOD_30MIN:  "30min",
	KLINE_PERIOD_60MIN:  "60min",
	KLINE_PERIOD_1DAY:   "1day",
	KLINE_PERIOD_1WEEK:  "1week",
	KLINE_PERIOD_1MONTH: "1mon",
	KLINE_PERIOD_1YEAR:  "1year",
}

const (
	HB_POINT_ACCOUNT = "point"
	HB_SPOT_ACCOUNT  = "spot"
)

type AccountInfo struct {
	Id    string
	Type  string
	State string
}

type HuoBiPro struct {
	httpClient        *http.Client
	baseUrl           string
	accountId         string
	accessKey         string
	secretKey         string
	ECDSAPrivateKey   string
	ws                *WsConn
	createWsLock      sync.Mutex
	wsTickerHandleMap map[string]func(*Ticker)
	wsDepthHandleMap  map[string]func(*Depth)
	wsTradeHandleMap  map[string]func(*Trade)
	wsKLineHandleMap  map[string]func(*Kline)
}

type HuoBiProSymbol struct {
	BaseCurrency string
	QuoteCurrency string
	PricePrecision float64
	AmountPrecision float64
	SymbolPartition string
	Symbol string
}

func NewHuoBiPro(client *http.Client, apikey, secretkey, accountId string) *HuoBiPro {
	hbpro := new(HuoBiPro)
	hbpro.baseUrl = "https://api.huobi.br.com"
	hbpro.httpClient = client
	hbpro.accessKey = apikey
	hbpro.secretKey = secretkey
	hbpro.accountId = accountId
	hbpro.wsDepthHandleMap = make(map[string]func(*Depth))
	hbpro.wsTickerHandleMap = make(map[string]func(*Ticker))
	hbpro.wsTradeHandleMap = make(map[string]func(*Trade))
	hbpro.wsKLineHandleMap = make(map[string]func(*Kline))
	return hbpro
}

/**
 *现货交易
 */
func NewHuoBiProSpot(client *http.Client, apikey, secretkey string) *HuoBiPro {
	hb := NewHuoBiPro(client, apikey, secretkey, "")
	accinfo, err := hb.GetAccountInfo(HB_SPOT_ACCOUNT)
	if err != nil {
		hb.accountId = ""
		//panic(err)
	} else {
		hb.accountId = accinfo.Id
		log.Println("account state :", accinfo.State)
	}
	return hb
}

/**
 * 点卡账户
 */
func NewHuoBiProPoint(client *http.Client, apikey, secretkey string) *HuoBiPro {
	hb := NewHuoBiPro(client, apikey, secretkey, "")
	accinfo, err := hb.GetAccountInfo(HB_POINT_ACCOUNT)
	if err != nil {
		panic(err)
	}
	hb.accountId = accinfo.Id
	log.Println("account state :", accinfo.State)
	return hb
}

func (hbpro *HuoBiPro) GetAccountInfo(acc string) (AccountInfo, error) {
	path := "/v1/account/accounts"
	params := &url.Values{}
	hbpro.buildPostForm("GET", path, params)

	//log.Println(hbpro.baseUrl + path + "?" + params.Encode())

	respmap, err := HttpGet(hbpro.httpClient, hbpro.baseUrl+path+"?"+params.Encode())
	if err != nil {
		return AccountInfo{}, err
	}
	//log.Println(respmap)
	if respmap["status"].(string) != "ok" {
		return AccountInfo{}, errors.New(respmap["err-code"].(string))
	}

	var info AccountInfo

	data := respmap["data"].([]interface{})
	for _, v := range data {
		iddata := v.(map[string]interface{})
		if iddata["type"].(string) == acc {
			info.Id = fmt.Sprintf("%.0f", iddata["id"])
			info.Type = acc
			info.State = iddata["state"].(string)
			break
		}
	}
	//log.Println(respmap)
	return info, nil
}

func (hbpro *HuoBiPro) GetAccount() (*Account, error) {
	path := fmt.Sprintf("/v1/account/accounts/%s/balance", hbpro.accountId)
	params := &url.Values{}
	params.Set("accountId-id", hbpro.accountId)
	hbpro.buildPostForm("GET", path, params)

	urlStr := hbpro.baseUrl + path + "?" + params.Encode()
	//println(urlStr)
	respmap, err := HttpGet(hbpro.httpClient, urlStr)

	if err != nil {
		return nil, err
	}

	//log.Println(respmap)

	if respmap["status"].(string) != "ok" {
		return nil, errors.New(respmap["err-code"].(string))
	}

	datamap := respmap["data"].(map[string]interface{})
	if datamap["state"].(string) != "working" {
		return nil, errors.New(datamap["state"].(string))
	}

	list := datamap["list"].([]interface{})
	acc := new(Account)
	acc.SubAccounts = make(map[Currency]SubAccount, 6)
	acc.Exchange = hbpro.GetExchangeName()

	subAccMap := make(map[Currency]*SubAccount)

	for _, v := range list {
		balancemap := v.(map[string]interface{})
		currencySymbol := balancemap["currency"].(string)
		currency := NewCurrency(currencySymbol, "")
		typeStr := balancemap["type"].(string)
		balance := ToFloat64(balancemap["balance"])
		if subAccMap[currency] == nil {
			subAccMap[currency] = new(SubAccount)
		}
		subAccMap[currency].Currency = currency
		switch typeStr {
		case "trade":
			subAccMap[currency].Amount = balance
		case "frozen":
			subAccMap[currency].ForzenAmount = balance
		}
	}

	for k, v := range subAccMap {
		acc.SubAccounts[k] = *v
	}

	return acc, nil
}

func (hbpro *HuoBiPro) placeOrder(amount, price string, pair CurrencyPair, orderType string) (string, error) {
	path := "/v1/order/orders/place"
	params := url.Values{}
	params.Set("account-id", hbpro.accountId)
	params.Set("amount", amount)
	params.Set("symbol", strings.ToLower(pair.ToSymbol("")))
	params.Set("type", orderType)

	switch orderType {
	case "buy-limit", "sell-limit":
		params.Set("price", price)
	}

	hbpro.buildPostForm("POST", path, &params)

	resp, err := HttpPostForm3(hbpro.httpClient, hbpro.baseUrl+path+"?"+params.Encode(), hbpro.toJson(params),
		map[string]string{"Content-Type": "application/json", "Accept-Language": "zh-cn"})
	if err != nil {
		return "", err
	}

	respmap := make(map[string]interface{})
	err = json.Unmarshal(resp, &respmap)
	if err != nil {
		return "", err
	}

	if respmap["status"].(string) != "ok" {
		return "", errors.New(respmap["err-code"].(string))
	}

	return respmap["data"].(string), nil
}

func (hbpro *HuoBiPro) LimitBuy(amount, price string, currency CurrencyPair) (*Order, error) {
	orderId, err := hbpro.placeOrder(amount, price, currency, "buy-limit")
	if err != nil {
		return nil, err
	}
	return &Order{
		Currency: currency,
		OrderID:  ToInt(orderId),
		OrderID2: orderId,
		Amount:   ToFloat64(amount),
		Price:    ToFloat64(price),
		Side:     BUY}, nil
}

func (hbpro *HuoBiPro) LimitSell(amount, price string, currency CurrencyPair) (*Order, error) {
	orderId, err := hbpro.placeOrder(amount, price, currency, "sell-limit")
	if err != nil {
		return nil, err
	}
	return &Order{
		Currency: currency,
		OrderID:  ToInt(orderId),
		OrderID2: orderId,
		Amount:   ToFloat64(amount),
		Price:    ToFloat64(price),
		Side:     SELL}, nil
}

func (hbpro *HuoBiPro) MarketBuy(amount, price string, currency CurrencyPair) (*Order, error) {
	orderId, err := hbpro.placeOrder(amount, price, currency, "buy-market")
	if err != nil {
		return nil, err
	}
	return &Order{
		Currency: currency,
		OrderID:  ToInt(orderId),
		OrderID2: orderId,
		Amount:   ToFloat64(amount),
		Price:    ToFloat64(price),
		Side:     BUY_MARKET}, nil
}

func (hbpro *HuoBiPro) MarketSell(amount, price string, currency CurrencyPair) (*Order, error) {
	orderId, err := hbpro.placeOrder(amount, price, currency, "sell-market")
	if err != nil {
		return nil, err
	}
	return &Order{
		Currency: currency,
		OrderID:  ToInt(orderId),
		OrderID2: orderId,
		Amount:   ToFloat64(amount),
		Price:    ToFloat64(price),
		Side:     SELL_MARKET}, nil
}

func (hbpro *HuoBiPro) parseOrder(ordmap map[string]interface{}) Order {
	ord := Order{
		OrderID:    ToInt(ordmap["id"]),
		OrderID2:   fmt.Sprint(ToInt(ordmap["id"])),
		Amount:     ToFloat64(ordmap["amount"]),
		Price:      ToFloat64(ordmap["price"]),
		DealAmount: ToFloat64(ordmap["field-amount"]),
		Fee:        ToFloat64(ordmap["field-fees"]),
		OrderTime:  ToInt(ordmap["created-at"]),
	}

	state := ordmap["state"].(string)
	switch state {
	case "submitted", "pre-submitted":
		ord.Status = ORDER_UNFINISH
	case "filled":
		ord.Status = ORDER_FINISH
	case "partial-filled":
		ord.Status = ORDER_PART_FINISH
	case "canceled", "partial-canceled":
		ord.Status = ORDER_CANCEL
	default:
		ord.Status = ORDER_UNFINISH
	}

	if ord.DealAmount > 0.0 {
		ord.AvgPrice = ToFloat64(ordmap["field-cash-amount"]) / ord.DealAmount
	}

	typeS := ordmap["type"].(string)
	switch typeS {
	case "buy-limit":
		ord.Side = BUY
	case "buy-market":
		ord.Side = BUY_MARKET
	case "sell-limit":
		ord.Side = SELL
	case "sell-market":
		ord.Side = SELL_MARKET
	}
	return ord
}

func (hbpro *HuoBiPro) GetOneOrder(orderId string, currency CurrencyPair) (*Order, error) {
	path := "/v1/order/orders/" + orderId
	params := url.Values{}
	hbpro.buildPostForm("GET", path, &params)
	respmap, err := HttpGet(hbpro.httpClient, hbpro.baseUrl+path+"?"+params.Encode())
	if err != nil {
		return nil, err
	}

	if respmap["status"].(string) != "ok" {
		return nil, errors.New(respmap["err-code"].(string))
	}

	datamap := respmap["data"].(map[string]interface{})
	order := hbpro.parseOrder(datamap)
	order.Currency = currency
	//log.Println(respmap)
	return &order, nil
}

func (hbpro *HuoBiPro) GetUnfinishOrders(currency CurrencyPair) ([]Order, error) {
	return hbpro.getOrders(queryOrdersParams{
		pair:   currency,
		states: "pre-submitted,submitted,partial-filled",
		size:   100,
		//direct:""
	})
}

func (hbpro *HuoBiPro) CancelOrder(orderId string, currency CurrencyPair) (bool, error) {
	path := fmt.Sprintf("/v1/order/orders/%s/submitcancel", orderId)
	params := url.Values{}
	hbpro.buildPostForm("POST", path, &params)
	resp, err := HttpPostForm3(hbpro.httpClient, hbpro.baseUrl+path+"?"+params.Encode(), hbpro.toJson(params),
		map[string]string{"Content-Type": "application/json", "Accept-Language": "zh-cn"})
	if err != nil {
		return false, err
	}

	var respmap map[string]interface{}
	err = json.Unmarshal(resp, &respmap)
	if err != nil {
		return false, err
	}

	if respmap["status"].(string) != "ok" {
		return false, errors.New(string(resp))
	}

	return true, nil
}

func (hbpro *HuoBiPro) GetOrderHistorys(currency CurrencyPair, currentPage, pageSize int) ([]Order, error) {
	return hbpro.getOrders(queryOrdersParams{
		pair:   currency,
		size:   pageSize,
		states: "partial-canceled,filled",
		direct: "next",
	})
}

type queryOrdersParams struct {
	types,
	startDate,
	endDate,
	states,
	from,
	direct string
	size int
	pair CurrencyPair
}

func (hbpro *HuoBiPro) getOrders(queryparams queryOrdersParams) ([]Order, error) {
	path := "/v1/order/orders"
	params := url.Values{}
	params.Set("symbol", strings.ToLower(queryparams.pair.ToSymbol("")))
	params.Set("states", queryparams.states)

	if queryparams.direct != "" {
		params.Set("direct", queryparams.direct)
	}

	if queryparams.size > 0 {
		params.Set("size", fmt.Sprint(queryparams.size))
	}

	hbpro.buildPostForm("GET", path, &params)
	respmap, err := HttpGet(hbpro.httpClient, fmt.Sprintf("%s%s?%s", hbpro.baseUrl, path, params.Encode()))
	if err != nil {
		return nil, err
	}

	if respmap["status"].(string) != "ok" {
		return nil, errors.New(respmap["err-code"].(string))
	}

	datamap := respmap["data"].([]interface{})
	var orders []Order
	for _, v := range datamap {
		ordmap := v.(map[string]interface{})
		ord := hbpro.parseOrder(ordmap)
		ord.Currency = queryparams.pair
		orders = append(orders, ord)
	}

	return orders, nil
}

func (hbpro *HuoBiPro) GetTicker(currencyPair CurrencyPair) (*Ticker, error) {
	url := hbpro.baseUrl + "/market/detail/merged?symbol=" + strings.ToLower(currencyPair.ToSymbol(""))
	respmap, err := HttpGet(hbpro.httpClient, url)
	if err != nil {
		return nil, err
	}

	if respmap["status"].(string) == "error" {
		return nil, errors.New(respmap["err-msg"].(string))
	}

	tickmap, ok := respmap["tick"].(map[string]interface{})
	if !ok {
		return nil, errors.New("tick assert error")
	}

	ticker := new(Ticker)
	ticker.Vol = ToFloat64(tickmap["amount"])
	ticker.Low = ToFloat64(tickmap["low"])
	ticker.High = ToFloat64(tickmap["high"])
	bid, isOk := tickmap["bid"].([]interface{})
	if isOk != true {
		return nil, errors.New("no bid")
	}
	ask, isOk := tickmap["ask"].([]interface{})
	if isOk != true {
		return nil, errors.New("no ask")
	}
	ticker.Buy = ToFloat64(bid[0])
	ticker.Sell = ToFloat64(ask[0])
	ticker.Last = ToFloat64(tickmap["close"])
	ticker.Date = ToUint64(respmap["ts"])

	return ticker, nil
}

func (hbpro *HuoBiPro) GetDepth(size int, currency CurrencyPair) (*Depth, error) {
	url := hbpro.baseUrl + "/market/depth?symbol=%s&type=step0"
	respmap, err := HttpGet(hbpro.httpClient, fmt.Sprintf(url, strings.ToLower(currency.ToSymbol(""))))
	if err != nil {
		return nil, err
	}

	if "ok" != respmap["status"].(string) {
		return nil, errors.New(respmap["err-msg"].(string))
	}

	tick, _ := respmap["tick"].(map[string]interface{})

	return hbpro.parseDepthData(tick), nil
}

//倒序
func (hbpro *HuoBiPro) GetKlineRecords(currency CurrencyPair, period, size, since int) ([]Kline, error) {
	url := hbpro.baseUrl + "/market/history/kline?period=%s&size=%d&symbol=%s"
	symbol := strings.ToLower(currency.AdaptUsdToUsdt().ToSymbol(""))
	periodS, isOk := _INERNAL_KLINE_PERIOD_CONVERTER[period]
	if isOk != true {
		periodS = "1min"
	}

	ret, err := HttpGet(hbpro.httpClient, fmt.Sprintf(url, periodS, size, symbol))
	if err != nil {
		return nil, err
	}

	data, ok := ret["data"].([]interface{})
	if !ok {
		return nil, errors.New("response format error")
	}

	var klines []Kline
	for _, e := range data {
		item := e.(map[string]interface{})
		klines = append(klines, Kline{
			Pair:      currency,
			Open:      ToFloat64(item["open"]),
			Close:     ToFloat64(item["close"]),
			High:      ToFloat64(item["high"]),
			Low:       ToFloat64(item["low"]),
			Vol:       ToFloat64(item["vol"]),
			Timestamp: int64(ToUint64(item["id"]))})
	}

	return klines, nil
}

//非个人，整个交易所的交易记录
func (hbpro *HuoBiPro) GetTrades(currencyPair CurrencyPair, since int64) ([]Trade, error) {
	panic("not implement")
}

type ecdsaSignature struct {
	R, S *big.Int
}

func (hbpro *HuoBiPro) buildPostForm(reqMethod, path string, postForm *url.Values) error {
	postForm.Set("AccessKeyId", hbpro.accessKey)
	postForm.Set("SignatureMethod", "HmacSHA256")
	postForm.Set("SignatureVersion", "2")
	postForm.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05"))
	domain := strings.Replace(hbpro.baseUrl, "https://", "", len(hbpro.baseUrl))
	payload := fmt.Sprintf("%s\n%s\n%s\n%s", reqMethod, domain, path, postForm.Encode())
	sign, _ := GetParamHmacSHA256Base64Sign(hbpro.secretKey, payload)
	postForm.Set("Signature", sign)

	/**
	p, _ := pem.Decode([]byte(hbpro.ECDSAPrivateKey))
	pri, _ := secp256k1_go.PrivKeyFromBytes(secp256k1_go.S256(), p.Bytes)
	signer, _ := pri.Sign([]byte(sign))
	signAsn, _ := asn1.Marshal(signer)
	priSign := base64.StdEncoding.EncodeToString(signAsn)
	postForm.Set("PrivateSignature", priSign)
	*/

	return nil
}

func (hbpro *HuoBiPro) toJson(params url.Values) string {
	parammap := make(map[string]string)
	for k, v := range params {
		parammap[k] = v[0]
	}
	jsonData, _ := json.Marshal(parammap)
	return string(jsonData)
}

func (hbpro *HuoBiPro) createWsConn() {

	onceWsConn.Do(func() {
		hbpro.ws = NewWsConn("wss://api.huobi.br.com/ws")
		hbpro.ws.Heartbeat(func() interface{} {
			return map[string]interface{}{
				"ping": time.Now().Unix()}
		}, 5*time.Second)
		hbpro.ws.ReConnect()
		hbpro.ws.ReceiveMessage(func(msg []byte) {
			gzipreader, _ := gzip.NewReader(bytes.NewReader(msg))
			data, _ := ioutil.ReadAll(gzipreader)
			datamap := make(map[string]interface{})
			//err := json.Unmarshal(data, &datamap)

			decoder := json.NewDecoder(bytes.NewBuffer(data))
			decoder.UseNumber() // UseNumber causes the Decoder to unmarshal a number into an interface{} as a Number instead of as a float64.

			err := decoder.Decode(&datamap)
			if err != nil {
				log.Println("json unmarshal error for ", string(data))
				return
			}

			if datamap["ping"] != nil {
				//log.Println(datamap)
				hbpro.ws.UpdateActivedTime()
				hbpro.ws.SendWriteJSON(map[string]interface{}{
					"pong": datamap["ping"]}) // 回应心跳
				return
			}

			if datamap["pong"] != nil { //
				hbpro.ws.UpdateActivedTime()
				return
			}

			if datamap["id"] != nil { //忽略订阅成功的回执消息
				log.Println(string(data))
				return
			}

			ch, isok := datamap["ch"].(string)
			if !isok {
				log.Println("error:", string(data))
				return
			}

			tick := datamap["tick"].(map[string]interface{})

			pair := hbpro.getPairFromChannel(ch)
			if strings.Contains(ch, ".detail") {
				if hbpro.wsTickerHandleMap[ch] != nil {
					tick := hbpro.parseTickerData(tick)
					tick.Pair = pair
					tick.Date = ToUint64(datamap["ts"])
					(hbpro.wsTickerHandleMap[ch])(tick)
					return
				}
			}

			if strings.Contains(ch, ".depth.step") {
				if hbpro.wsDepthHandleMap[ch] != nil {
					depth := hbpro.parseDepthData(tick)
					depth.Pair = pair
					(hbpro.wsDepthHandleMap[ch])(depth)
					return
				}
			}

			if strings.Contains(ch, ".trade.detail") {
				trades := hbpro.parseTradeData(tick)
				//反向是为了和app服务端顺序一致
				for i := len(trades) - 1; i >= 0; i-- {
					trades[i].Pair = pair
					(hbpro.wsTradeHandleMap[ch])(trades[i])
				}
			}

			if hbpro.wsKLineHandleMap[ch] != nil {
				kline := hbpro.parseWsKLineData(tick)
				kline.Pair = pair
				(hbpro.wsKLineHandleMap[ch])(kline)
				return
			}

			//log.Println(string(data))
		})
	})

}

func Between(str, starting, ending string) string {
	s := strings.Index(str, starting)
	if s < 0 {
		return ""
	}
	s += len(starting)
	e := strings.Index(str[s:], ending)
	if e < 0 {
		return ""
	}
	return str[s: s+e]
}

func (hbpro *HuoBiPro) getPairFromChannel(ch string) CurrencyPair {


	var currA, currB, symbol string

	//命中topic 类型  Trade Detail
	if s := regexp.MustCompile(`market.(.*).kline.*`).FindStringSubmatch(ch); len(s) > 1 {
		symbol = s[1]
	} else if s := regexp.MustCompile(`market.(.*).depth.*`).FindStringSubmatch(ch); len(s) > 1 {
		symbol = s[1]
	} else if s := regexp.MustCompile(`market.(.*).trade.detail`).FindStringSubmatch(ch); len(s) > 1 {
		symbol = s[1]
	} else if s := regexp.MustCompile(`market.(.*).detail	`).FindStringSubmatch(ch); len(s) > 1 {
		symbol = s[1]
	} else if s := regexp.MustCompile(`orders.(.*)`).FindStringSubmatch(ch); len(s) > 1 {
		symbol = s[1]
	}

	if strings.HasSuffix(symbol, "usdt") {
		currB = "usdt"
	} else if strings.HasSuffix(symbol, "husd") {
		currB = "husd"
	} else if strings.HasSuffix(symbol, "btc") {
		currB = "btc"
	} else if strings.HasSuffix(symbol, "eth") {
		currB = "eth"
	} else if strings.HasSuffix(symbol, "ht") {
		currB = "ht"
	}

	currA = strings.TrimSuffix(symbol, currB)



	a := NewCurrency(currA, "")
	b := NewCurrency(currB, "")
	pair := NewCurrencyPair(a, b)

	return pair
}

func (hbpro *HuoBiPro) parseTickerData(tick map[string]interface{}) *Ticker {
	t := new(Ticker)

	t.Last = ToFloat64(tick["close"])
	t.Low = ToFloat64(tick["low"])
	t.Vol = ToFloat64(tick["vol"])
	t.High = ToFloat64(tick["high"])
	return t
}

func (hbpro *HuoBiPro) parseDepthData(tick map[string]interface{}) *Depth {
	bids, _ := tick["bids"].([]interface{})
	asks, _ := tick["asks"].([]interface{})

	depth := new(Depth)
	for _, r := range asks {
		var dr DepthRecord
		rr := r.([]interface{})
		dr.Price = ToFloat64(rr[0])
		dr.Amount = ToFloat64(rr[1])
		depth.AskList = append(depth.AskList, dr)
	}

	for _, r := range bids {
		var dr DepthRecord
		rr := r.([]interface{})
		dr.Price = ToFloat64(rr[0])
		dr.Amount = ToFloat64(rr[1])
		depth.BidList = append(depth.BidList, dr)
	}

	sort.Sort(sort.Reverse(depth.AskList))

	return depth
}

func (hbpro *HuoBiPro) parseTradeData(tick map[string]interface{}) (trades []*Trade) {

	arr, _ := tick["data"].([]interface{})

	//经过发现，高并发时,火币的接口中返回的数据中有重复值，BigId相同
	tMap := make(map[string]*Trade)
	for _, t := range arr {
		trade := new(Trade)
		z := t.(map[string]interface{})
		trade.BigId = z["id"].(json.Number).String()
		trade.Amount = ToFloat64(z["amount"])
		trade.Price = ToFloat64(z["price"])
		trade.Date, _ = z["ts"].(json.Number).Int64()
		direction := z["direction"].(string)
		if direction == "buy" {
			trade.Type = BUY
		} else {
			trade.Type = SELL
		}
		tMap[trade.BigId] = trade
	}

	//利用map特性去重，然后转换成切片
	for _, v := range tMap {
		trades = append(trades, v)
	}

	return trades
}
func (hbpro *HuoBiPro) parseWsKLineData(tick map[string]interface{}) *Kline {
	return &Kline{
		Open:      ToFloat64(tick["open"]),
		Close:     ToFloat64(tick["close"]),
		High:      ToFloat64(tick["high"]),
		Low:       ToFloat64(tick["low"]),
		Vol:       ToFloat64(tick["vol"]),
		Timestamp: int64(ToUint64(tick["id"]))}
}

func (hbpro *HuoBiPro) GetExchangeName() string {
	return HUOBI_PRO
}

func (hbpro *HuoBiPro) GetCurrenciesList() ([]string, error)  {
	url := hbpro.baseUrl + "/v1/common/currencys"

	ret, err := HttpGet(hbpro.httpClient, url)
	if err != nil {
		return nil, err
	}

	data, ok := ret["data"].([]interface{})
	if !ok {
		return nil, errors.New("response format error")
	}
	fmt.Println(data)
	return nil, nil
}

func (hbpro *HuoBiPro) GetCurrenciesPrecision() ([]HuoBiProSymbol, error)  {
	url := hbpro.baseUrl + "/v1/common/symbols"

	ret, err := HttpGet(hbpro.httpClient, url)
	if err != nil {
		return nil, err
	}

	data, ok := ret["data"].([]interface{})
	if !ok {
		return nil, errors.New("response format error")
	}
	var Symbols []HuoBiProSymbol
	for _, v := range data {
		_sym := v.(map[string]interface{})
		var sym HuoBiProSymbol
		sym.BaseCurrency = _sym["base-currency"].(string)
		sym.QuoteCurrency = _sym["quote-currency"].(string)
		sym.PricePrecision = _sym["price-precision"].(float64)
		sym.AmountPrecision = _sym["amount-precision"].(float64)
		sym.SymbolPartition = _sym["symbol-partition"].(string)
		sym.Symbol = _sym["symbol"].(string)
		Symbols = append(Symbols, sym)
	}
	//fmt.Println(Symbols)
	return Symbols, nil
}

func (hbpro *HuoBiPro) GetTickerWithWs(pair CurrencyPair, handle func(ticker *Ticker)) error {
	hbpro.createWsConn()
	sub := fmt.Sprintf("market.%s.detail", strings.ToLower(pair.ToSymbol("")))
	hbpro.wsTickerHandleMap[sub] = handle
	return hbpro.ws.Subscribe(map[string]interface{}{
		"id":  1,
		"sub": sub})
}

func (hbpro *HuoBiPro) GetDepthWithWs(pair CurrencyPair, handle func(dep *Depth)) error {
	hbpro.createWsConn()
	sub := fmt.Sprintf("market.%s.depth.step0", strings.ToLower(pair.ToSymbol("")))
	hbpro.wsDepthHandleMap[sub] = handle
	return hbpro.ws.Subscribe(map[string]interface{}{
		"id":  2,
		"sub": sub})
}

func (hbpro *HuoBiPro) GetTradeWithWs(pair CurrencyPair, handle func(dep *Trade)) error {
	hbpro.createWsConn()
	sub := fmt.Sprintf("market.%s.trade.detail", strings.ToLower(pair.ToSymbol("")))
	hbpro.wsTradeHandleMap[sub] = handle
  return hbpro.ws.Subscribe(map[string]interface{}{
		"id":  3,
		"sub": sub})
}

func (hbpro *HuoBiPro) GetKLineWithWs(pair CurrencyPair, period int, handle func(kline *Kline)) error {
	hbpro.createWsConn()
	periodS, isOk := _INERNAL_KLINE_PERIOD_CONVERTER[period]
	if isOk != true {
		periodS = "1min"
	}

	sub := fmt.Sprintf("market.%s.kline.%s", strings.ToLower(pair.ToSymbol("")), periodS)
	hbpro.wsKLineHandleMap[sub] = handle
	return hbpro.ws.Subscribe(map[string]interface{}{
		"id":  3,
		"sub": sub})
}
