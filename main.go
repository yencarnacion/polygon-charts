// main.go
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

/*────────────────────  configuration  ────────────────────*/

var (
	apiKeyFlag = flag.String("apikey", "", "Polygon.io API key (overrides .env)")
	portFlag   = flag.Int("port", 0,  "HTTP port (overrides .env)")
)

var (
	polygonAPIKey string
	fmpAPIKey     string // New for FMP
	listenPort    int
)

/*────────────────────  embedded HTML  ────────────────────*/

//go:embed index.html
var indexHTML string

/*────────────────────  data structures  ──────────────────*/

type polygonBar struct {
	T int64   `json:"t"` // unix-ms
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V float64 `json:"v"` // volume
}
type polygonResp struct{ Results []polygonBar `json:"results"` }

type candlePoint struct {
	Time   int64   `json:"time"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
}
type linePoint struct {
	Time  int64   `json:"time"`
	Value float64 `json:"value"`
}
type payload struct {
	Candles []candlePoint `json:"candles"`
	Volume  []linePoint   `json:"volume"` // NEW
	MinCandles []candlePoint `json:"minCandles"`
	MinVolume  []linePoint   `json:"minVolume"`
	VWAP    []linePoint   `json:"vwap"`
	SMA9    []linePoint   `json:"sma"`
}

/* ─── New structs for APIs ────────────────────────────── */

type PolygonTickerDetails struct {
  Results struct {
    MarketCap float64 `json:"market_cap"`
  } `json:"results"`
}

type FMPFloat struct {
  Symbol              string `json:"symbol"`
  FloatShares         float64 `json:"floatShares"`
}

/*────────────────────  helpers  ─────────────────────────*/

func queryPolygon(sym string, mult int, span, from, to string) ([]polygonBar, error) {
	url := fmt.Sprintf(
		"https://api.polygon.io/v2/aggs/ticker/%s/range/%d/%s/%s/%s?adjusted=true&sort=asc&apiKey=%s",
		sym, mult, span, from, to, polygonAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Polygon: %s", resp.Status)
	}
	var pr polygonResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return pr.Results, nil
}

/* ─── New: Query Polygon Ticker Details ──────────────────── */
func queryPolygonTickerDetails(ticker string) (*PolygonTickerDetails, error) {
	url := fmt.Sprintf("https://api.polygon.io/v3/reference/tickers/%s?apiKey=%s", ticker, polygonAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Polygon Ticker Details: %s", resp.Status)
	}
	var details PolygonTickerDetails
	if err := json.NewDecoder(resp.Body).Decode(&details); err != nil {
		return nil, err
	}
	return &details, nil
}

/* ─── New: Query FMP Share Float ──────────────────────────── */
func queryFMPFloat(ticker string) ([]FMPFloat, error) {
	url := fmt.Sprintf("https://financialmodelingprep.com/api/v4/shares_float?symbol=%s&apikey=%s", ticker, fmpAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FMP Float: %s", resp.Status)
	}
	var floats []FMPFloat
	if err := json.NewDecoder(resp.Body).Decode(&floats); err != nil {
		return nil, err
	}
	return floats, nil
}

func openBrowser(u string) {
	if err := exec.Command("google-chrome", "--new-tab", u).Start(); err != nil {
		_ = exec.Command("xdg-open", u).Start()
	}
}

/*────────────────────  HTTP handlers  ───────────────────*/

func rootHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, indexHTML)
}

func candlesHandler(w http.ResponseWriter, r *http.Request) {
	/* ---------- query params ---------- */
	q       := r.URL.Query()
	symbol  := strings.ToUpper(q.Get("ticker"))
	tf      := strings.ToLower(strings.TrimSpace(q.Get("timeframe")))
	dateStr := q.Get("date") // YYYY-MM-DD
	extended := q.Get("extended") == "true"

	if symbol == "" || tf == "" || dateStr == "" {
		http.Error(w, "ticker, timeframe, date required", 400)
		return
	}

	/* timeframe → multiplier / span */
	var mult int; var span string
	switch tf {
	case "1m","1min":   mult,span = 1, "minute"
	case "2m","2min":   mult,span = 2, "minute"
	case "3m","3min":   mult,span = 3, "minute"
	case "5m","5min":   mult,span = 5, "minute"
	case "10m","10min": mult,span =10, "minute"
	case "15m","15min": mult,span =15, "minute"
	case "30m","30min": mult,span =30, "minute"
	case "1h","1hr":    mult,span = 1, "hour"
	default:
		http.Error(w, "unsupported timeframe", 400); return
	}

	/* ---------- pull chosen-tf bars ---------- */
	bars, err := queryPolygon(symbol, mult, span, dateStr, dateStr)
	if err != nil { http.Error(w, err.Error(), 502); return }

	loc, _ := time.LoadLocation("America/New_York")
	candles := make([]candlePoint, 0, len(bars))
	vol     := make([]linePoint,   0, len(bars))

	for _, b := range bars {
		ts := time.UnixMilli(b.T).In(loc)
		h,m := ts.Hour(), ts.Minute()
		if !extended && (h < 9 || (h==9 && m<30) || h >= 16) { continue } // RTH filter if not extended

		candles = append(candles, candlePoint{
			Time:b.T/1000, Open:b.O, High:b.H, Low:b.L, Close:b.C})
		vol = append(vol, linePoint{Time:b.T/1000, Value:b.V})
	}

	/* ---------- SMA-9 ---------- */
	var sum float64
	sma := make([]linePoint,0,len(candles))
	for i,c := range candles{
		sum += c.Close
		if i>=9 { sum-=candles[i-9].Close }
		if i>=8 { sma = append(sma,linePoint{Time:c.Time,Value:sum/9}) }
	}

	/* ---------- VWAP, MinCandles, MinVolume (1-min bars) ---------- */
	minBars, err := queryPolygon(symbol,1,"minute",dateStr,dateStr)
	if err != nil { http.Error(w, err.Error(), 502); return }

	var cumPV,cumV float64
	vwap := make([]linePoint,0,len(minBars))
	minCandles := make([]candlePoint, 0, len(minBars))
	minVol := make([]linePoint, 0, len(minBars))
	for _,b := range minBars{
		ts:=time.UnixMilli(b.T).In(loc); h,m:=ts.Hour(),ts.Minute()
		if !extended && (h<9||(h==9&&m<30)||h>=16){continue}
		typ := (b.H+b.L+b.C)/3
		cumPV += typ*b.V; cumV += b.V
		if cumV>0 { vwap=append(vwap,linePoint{Time:b.T/1000,Value:cumPV/cumV}) }
		minCandles = append(minCandles, candlePoint{
			Time:b.T/1000, Open:b.O, High:b.H, Low:b.L, Close:b.C})
		minVol = append(minVol, linePoint{Time:b.T/1000, Value:b.V})
	}

	out := payload{Candles:candles, Volume:vol, MinCandles:minCandles, MinVolume:minVol, VWAP:vwap, SMA9:sma}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

/* ─── New Handler: Ticker Details (Polygon Proxy) ────────── */
func tickerDetailsHandler(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.URL.Query().Get("ticker"))
	if ticker == "" {
		http.Error(w, "ticker required", 400)
		return
	}

	details, err := queryPolygonTickerDetails(ticker)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(details)
}

/* ─── New Handler: Share Float (FMP Proxy) ────────────────── */
func shareFloatHandler(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.URL.Query().Get("ticker"))
	if ticker == "" {
		http.Error(w, "ticker required", 400)
		return
	}

	floats, err := queryFMPFloat(ticker)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(floats)
}

/*────────────────────  main  ───────────────────────────*/

func main() {
	_ = godotenv.Load()
	flag.Parse()

	polygonAPIKey = *apiKeyFlag
	if polygonAPIKey=="" { polygonAPIKey = os.Getenv("POLYGON_API_KEY") }
	if polygonAPIKey=="" { log.Fatal("Polygon API key missing") }

	fmpAPIKey = os.Getenv("FMP_API_KEY")
	if fmpAPIKey == "" { log.Fatal("FMP API key missing") }

	if *portFlag!=0 { listenPort=*portFlag
	} else if p:=os.Getenv("PORT"); p!="" { fmt.Sscanf(p,"%d",&listenPort) }
	if listenPort==0 { listenPort=8081 }

	http.HandleFunc("/", rootHandler)
	http.HandleFunc("/api/candles", candlesHandler)
	http.HandleFunc("/api/ticker-details", tickerDetailsHandler) // New
	http.HandleFunc("/api/share-float", shareFloatHandler)       // New

	addr := fmt.Sprintf(":%d", listenPort)
	go func(){ time.Sleep(500*time.Millisecond); openBrowser("http://localhost"+addr) }()

	log.Printf("Serving on http://localhost%s …", addr)
	log.Fatal(http.ListenAndServe(addr,nil))
}
