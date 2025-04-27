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
	VWAP    []linePoint   `json:"vwap"`
	SMA9    []linePoint   `json:"sma"`
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
		if h < 9 || (h==9 && m<30) || h >= 16 { continue } // RTH filter

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

	/* ---------- VWAP (1-min bars) ---------- */
	minBars, err := queryPolygon(symbol,1,"minute",dateStr,dateStr)
	if err != nil { http.Error(w, err.Error(), 502); return }

	var cumPV,cumV float64
	vwap := make([]linePoint,0,len(minBars))
	for _,b := range minBars{
		ts:=time.UnixMilli(b.T).In(loc); h,m:=ts.Hour(),ts.Minute()
		if h<9||(h==9&&m<30)||h>=16{continue}
		typ := (b.H+b.L+b.C)/3
		cumPV += typ*b.V; cumV += b.V
		if cumV>0 { vwap=append(vwap,linePoint{Time:b.T/1000,Value:cumPV/cumV}) }
	}

	out := payload{Candles:candles, Volume:vol, VWAP:vwap, SMA9:sma}
	w.Header().Set("Content-Type","application/json")
	json.NewEncoder(w).Encode(out)
}

/*────────────────────  main  ───────────────────────────*/

func main() {
	_ = godotenv.Load()
	flag.Parse()

	polygonAPIKey = *apiKeyFlag
	if polygonAPIKey=="" { polygonAPIKey = os.Getenv("POLYGON_API_KEY") }
	if polygonAPIKey=="" { log.Fatal("Polygon API key missing") }

	if *portFlag!=0 { listenPort=*portFlag
	} else if p:=os.Getenv("PORT"); p!="" { fmt.Sscanf(p,"%d",&listenPort) }
	if listenPort==0 { listenPort=8080 }

	http.HandleFunc("/", rootHandler)
	http.HandleFunc("/api/candles", candlesHandler)

	addr := fmt.Sprintf(":%d", listenPort)
	go func(){ time.Sleep(500*time.Millisecond); openBrowser("http://localhost"+addr) }()

	log.Printf("Serving on http://localhost%s …", addr)
	log.Fatal(http.ListenAndServe(addr,nil))
}
