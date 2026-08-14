package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeTradingViewConnection struct {
	writes []map[string]any
	reads  []any
	closed bool
}

func (f *fakeTradingViewConnection) WriteJSON(value any) error {
	encoded, _ := json.Marshal(value)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	f.writes = append(f.writes, decoded)
	return nil
}

func (f *fakeTradingViewConnection) ReadJSON(value any) error {
	if len(f.reads) == 0 {
		return io.EOF
	}
	encoded, _ := json.Marshal(f.reads[0])
	f.reads = f.reads[1:]
	return json.Unmarshal(encoded, value)
}

func (f *fakeTradingViewConnection) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeTradingViewConnection) SetWriteDeadline(time.Time) error { return nil }
func (f *fakeTradingViewConnection) SetReadLimit(int64)               {}
func (f *fakeTradingViewConnection) Close() error {
	f.closed = true
	return nil
}

func newTestTradingViewClient(t *testing.T, current string) (*tradingViewClient, *fakeTradingViewConnection, func()) {
	t.Helper()
	connection := &fakeTradingViewConnection{reads: []any{
		map[string]any{"method": "Runtime.consoleAPICalled"},
		map[string]any{
			"id": 1,
			"result": map[string]any{
				"result": map[string]any{
					"value": map[string]any{"current": current},
				},
			},
		},
	}}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/list" {
			http.NotFound(w, r)
			return
		}
		host := strings.TrimPrefix(server.URL, "http://")
		_ = json.NewEncoder(w).Encode([]tradingViewCDPTarget{{
			ID:                   "page-1",
			Type:                 "page",
			Title:                "TradingView",
			URL:                  "https://www.tradingview.com/chart/abc/",
			WebSocketDebuggerURL: "ws://" + host + "/devtools/page/page-1",
		}})
	}))

	parsed, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	client := &tradingViewClient{
		enabled: true,
		cdpURL:  parsed,
		timeout: time.Second,
		http:    server.Client(),
		dial: func(context.Context, string, http.Header) (tradingViewCDPConnection, *http.Response, error) {
			return connection, nil, nil
		},
	}
	return client, connection, server.Close
}

type stubTradingViewController struct {
	enabled bool
	status  tradingViewStatus
	symbol  string
	err     error
}

func (s *stubTradingViewController) Enabled() bool { return s.enabled }
func (s *stubTradingViewController) Status(context.Context) tradingViewStatus {
	return s.status
}
func (s *stubTradingViewController) SetSymbol(_ context.Context, symbol string) error {
	s.symbol = symbol
	return s.err
}

func TestInjectTradingViewIntegrationScript(t *testing.T) {
	input := "<html><body>chart</body></html>"
	got := injectTradingViewIntegrationScript(input)
	if strings.Count(got, tradingViewIntegrationScript) != 1 ||
		!strings.Contains(got, `src="`+tradingViewIntegrationScript+`"></script>`) ||
		strings.Index(got, tradingViewIntegrationScript) > strings.Index(got, "</body>") {
		t.Fatalf("script was not injected before body close: %q", got)
	}
	if again := injectTradingViewIntegrationScript(got); again != got {
		t.Fatal("script injection must be idempotent")
	}
	if !strings.Contains(tradingViewIntegrationJS, "#runSymbolAction") ||
		!strings.Contains(tradingViewIntegrationJS, tradingViewTickerEndpoint) {
		t.Fatal("embedded integration script is missing the click fan-out")
	}
}

func TestParseLoopbackHTTPURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:9222", "http://localhost:9222", "http://[::1]:9222"} {
		if _, err := parseLoopbackHTTPURL(raw); err != nil {
			t.Fatalf("%s should be accepted: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://example.com:9222", "ftp://127.0.0.1:9222", "http://user@127.0.0.1:9222"} {
		if _, err := parseLoopbackHTTPURL(raw); err == nil {
			t.Fatalf("%s should be rejected", raw)
		}
	}
}

func TestSelectTradingViewChartTarget(t *testing.T) {
	target, ok := selectTradingViewChartTarget([]tradingViewCDPTarget{
		{Type: "page", Title: "TradingView", URL: "app://tradingview/window", WebSocketDebuggerURL: "ws://127.0.0.1:9222/devtools/page/fallback"},
		{Type: "page", Title: "Chart", URL: "https://www.tradingview.com/chart/abc/", WebSocketDebuggerURL: "ws://127.0.0.1:9222/devtools/page/chart"},
	})
	if !ok || !strings.HasSuffix(target.WebSocketDebuggerURL, "/chart") {
		t.Fatalf("selected target = %#v, ok=%v", target, ok)
	}
	if _, ok := selectTradingViewChartTarget([]tradingViewCDPTarget{{
		Type: "page", URL: "https://www.tradingview.com/chart/abc/", WebSocketDebuggerURL: "ws://192.0.2.10:9222/devtools/page/chart",
	}}); ok {
		t.Fatal("non-loopback WebSocket target must be rejected")
	}
}

func TestTradingViewSetSymbol(t *testing.T) {
	client, connection, closeServer := newTestTradingViewClient(t, "NASDAQ:AAPL")
	defer closeServer()
	if err := client.SetSymbol(context.Background(), " aapl "); err != nil {
		t.Fatal(err)
	}
	if len(connection.writes) != 1 || connection.writes[0]["method"] != "Runtime.evaluate" {
		t.Fatalf("unexpected CDP write: %#v", connection.writes)
	}
	params, _ := connection.writes[0]["params"].(map[string]any)
	expression, _ := params["expression"].(string)
	if !strings.Contains(expression, `chart.setSymbol("AAPL", {})`) {
		t.Fatalf("expression does not set normalized symbol: %s", expression)
	}
	if !connection.closed {
		t.Fatal("CDP connection was not closed")
	}
}

func TestTradingViewSetSymbolDetectsWrongResult(t *testing.T) {
	client, _, closeServer := newTestTradingViewClient(t, "NASDAQ:MSFT")
	defer closeServer()
	if err := client.SetSymbol(context.Background(), "AAPL"); err == nil || !strings.Contains(err.Error(), "reported") {
		t.Fatalf("expected symbol mismatch, got %v", err)
	}
}

func TestTradingViewExpressionEscapesInput(t *testing.T) {
	expression := tradingViewSetSymbolExpression(`AAPL\"; window.bad = true; //`)
	if !strings.Contains(expression, `AAPL\\\"; window.bad = true; //`) {
		t.Fatalf("input was not JSON escaped: %s", expression)
	}
}

func TestTradingViewHandlersRequireLoopback(t *testing.T) {
	status := httptest.NewRecorder()
	statusRequest := httptest.NewRequest(http.MethodGet, tradingViewStatusEndpoint, nil)
	tradingViewStatusHandler(status, statusRequest)
	if status.Code != http.StatusForbidden {
		t.Fatalf("remote status code=%d", status.Code)
	}

	ticker := httptest.NewRecorder()
	tickerRequest := httptest.NewRequest(http.MethodPost, tradingViewTickerEndpoint, strings.NewReader(`{"symbol":"AAPL"}`))
	tradingViewTickerHandler(ticker, tickerRequest)
	if ticker.Code != http.StatusForbidden {
		t.Fatalf("remote ticker code=%d", ticker.Code)
	}
}

func TestTradingViewStatusHandler(t *testing.T) {
	stub := &stubTradingViewController{enabled: true, status: tradingViewStatus{Enabled: true, Connected: true}}
	previous := tradingViewClientForRequest
	tradingViewClientForRequest = func() tradingViewController { return stub }
	defer func() { tradingViewClientForRequest = previous }()

	request := httptest.NewRequest(http.MethodGet, tradingViewStatusEndpoint, nil)
	request.RemoteAddr = "127.0.0.1:55000"
	response := httptest.NewRecorder()
	tradingViewStatusHandler(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"connected":true`) {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTradingViewTickerHandler(t *testing.T) {
	stub := &stubTradingViewController{enabled: true}
	previous := tradingViewClientForRequest
	tradingViewClientForRequest = func() tradingViewController { return stub }
	defer func() { tradingViewClientForRequest = previous }()

	request := httptest.NewRequest(http.MethodPost, tradingViewTickerEndpoint, strings.NewReader(`{"symbol":" aapl "}`))
	request.RemoteAddr = "127.0.0.1:55000"
	response := httptest.NewRecorder()
	tradingViewTickerHandler(response, request)
	if response.Code != http.StatusOK || stub.symbol != "AAPL" || !strings.Contains(response.Body.String(), `"symbol":"AAPL"`) {
		t.Fatalf("code=%d selected=%q body=%s", response.Code, stub.symbol, response.Body.String())
	}
}

func TestTradingViewTickerHandlerDisabled(t *testing.T) {
	stub := &stubTradingViewController{}
	previous := tradingViewClientForRequest
	tradingViewClientForRequest = func() tradingViewController { return stub }
	defer func() { tradingViewClientForRequest = previous }()

	request := httptest.NewRequest(http.MethodPost, tradingViewTickerEndpoint, strings.NewReader(`{"symbol":"AAPL"}`))
	request.RemoteAddr = "[::1]:55000"
	response := httptest.NewRecorder()
	tradingViewTickerHandler(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", response.Code)
	}
}
