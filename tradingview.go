package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultTradingViewCDPURL     = "http://127.0.0.1:9222"
	defaultTradingViewTimeout    = 3 * time.Second
	maxTradingViewCDPBody        = 1 << 20
	tradingViewIntegrationScript = "/assets/tradingview-integration.js"
	tradingViewStatusEndpoint    = "/api/integrations/tradingview/status"
	tradingViewTickerEndpoint    = "/api/integrations/tradingview/ticker"
)

var tradingViewSymbolPattern = regexp.MustCompile(`^[A-Z][A-Z0-9.\-]{0,15}$`)

//go:embed tradingview-integration.js
var tradingViewIntegrationJS string

type tradingViewStatus struct {
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

type tradingViewController interface {
	Enabled() bool
	Status(context.Context) tradingViewStatus
	SetSymbol(context.Context, string) error
}

type tradingViewCDPTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type tradingViewCDPConnection interface {
	WriteJSON(any) error
	ReadJSON(any) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	SetReadLimit(int64)
	Close() error
}

type tradingViewCDPDialFunc func(context.Context, string, http.Header) (tradingViewCDPConnection, *http.Response, error)

type tradingViewClient struct {
	enabled   bool
	cdpURL    *url.URL
	timeout   time.Duration
	http      *http.Client
	dial      tradingViewCDPDialFunc
	configErr error
	mu        sync.Mutex
}

var (
	tradingViewClientInitMu sync.Mutex
	tradingViewClientCached tradingViewController
	tradingViewClientForRequest = func() tradingViewController {
		tradingViewClientInitMu.Lock()
		defer tradingViewClientInitMu.Unlock()
		if tradingViewClientCached == nil {
			// The first request arrives after main has loaded .env with godotenv.
			tradingViewClientCached = newTradingViewClientFromEnv()
		}
		return tradingViewClientCached
	}
)

func init() {
	chartHTML = injectTradingViewIntegrationScript(chartHTML)
	http.HandleFunc(tradingViewIntegrationScript, tradingViewIntegrationScriptHandler)
	http.HandleFunc(tradingViewStatusEndpoint, tradingViewStatusHandler)
	http.HandleFunc(tradingViewTickerEndpoint, tradingViewTickerHandler)
}

func injectTradingViewIntegrationScript(html string) string {
	if strings.Contains(html, tradingViewIntegrationScript) {
		return html
	}
	tag := `<script src="` + tradingViewIntegrationScript + `"></script>`
	lower := strings.ToLower(html)
	if index := strings.LastIndex(lower, "</body>"); index >= 0 {
		return html[:index] + tag + "\n" + html[index:]
	}
	return html + "\n" + tag + "\n"
}

func tradingViewIntegrationScriptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, tradingViewIntegrationJS)
	}
}

func newTradingViewClientFromEnv() tradingViewController {
	enabled := false
	var configErr error
	if raw := strings.TrimSpace(os.Getenv("POLYGON_CHARTS_TRADINGVIEW_ENABLED")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			configErr = errors.Join(configErr, fmt.Errorf("POLYGON_CHARTS_TRADINGVIEW_ENABLED: %w", err))
		} else {
			enabled = value
		}
	}

	base := strings.TrimSpace(os.Getenv("POLYGON_CHARTS_TRADINGVIEW_CDP_URL"))
	if base == "" {
		base = defaultTradingViewCDPURL
	}
	parsed, err := parseLoopbackHTTPURL(base)
	if err != nil {
		configErr = errors.Join(configErr, fmt.Errorf("POLYGON_CHARTS_TRADINGVIEW_CDP_URL: %w", err))
		parsed, _ = parseLoopbackHTTPURL(defaultTradingViewCDPURL)
	}

	timeout := defaultTradingViewTimeout
	if raw := strings.TrimSpace(os.Getenv("POLYGON_CHARTS_TRADINGVIEW_TIMEOUT")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			configErr = errors.Join(configErr, errors.New("POLYGON_CHARTS_TRADINGVIEW_TIMEOUT must be a positive duration"))
		} else {
			timeout = value
		}
	}

	transport := &http.Transport{}
	if baseTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = baseTransport.Clone()
	}
	transport.Proxy = nil
	dialer := &websocket.Dialer{HandshakeTimeout: timeout, Proxy: nil}
	return &tradingViewClient{
		enabled:   enabled,
		cdpURL:    parsed,
		timeout:   timeout,
		http:      &http.Client{Timeout: timeout, Transport: transport},
		configErr: configErr,
		dial: func(ctx context.Context, endpoint string, header http.Header) (tradingViewCDPConnection, *http.Response, error) {
			return dialer.DialContext(ctx, endpoint, header)
		},
	}
}

func parseLoopbackHTTPURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("must use an unauthenticated loopback host such as 127.0.0.1")
	}
	return parsed, nil
}

func loopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validLoopbackWebSocketURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return loopbackHost(parsed.Hostname())
}

func (c *tradingViewClient) Enabled() bool {
	return c != nil && c.enabled
}

func (c *tradingViewClient) Status(ctx context.Context) tradingViewStatus {
	status := tradingViewStatus{Enabled: c.Enabled()}
	if !status.Enabled {
		return status
	}
	if c.configErr != nil {
		status.Error = c.configErr.Error()
		return status
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	_, err := c.findChartTarget(ctx)
	status.Connected = err == nil
	if err != nil {
		status.Error = err.Error()
	}
	return status
}

func (c *tradingViewClient) SetSymbol(ctx context.Context, symbol string) error {
	if !c.Enabled() {
		return errors.New("TradingView Desktop integration is disabled")
	}
	if c.configErr != nil {
		return c.configErr
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if !tradingViewSymbolPattern.MatchString(symbol) {
		return errors.New("invalid symbol")
	}

	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	// Multiple chart tabs can click at once. Keep commands ordered so an older
	// request cannot finish after a newer selection.
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setSymbol(ctx, symbol)
}

func (c *tradingViewClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

func (c *tradingViewClient) setSymbol(ctx context.Context, symbol string) error {
	target, err := c.findChartTarget(ctx)
	if err != nil {
		return err
	}
	if !validLoopbackWebSocketURL(target.WebSocketDebuggerURL) {
		return errors.New("TradingView returned a non-loopback CDP WebSocket URL")
	}

	connection, handshakeResponse, err := c.dial(ctx, target.WebSocketDebuggerURL, nil)
	if handshakeResponse != nil && handshakeResponse.Body != nil {
		defer handshakeResponse.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connect to TradingView CDP target: %w", err)
	}
	defer connection.Close()
	connection.SetReadLimit(maxTradingViewCDPBody)

	deadline := time.Now().Add(c.timeout)
	if value, ok := ctx.Deadline(); ok {
		deadline = value
	}
	if err := connection.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set TradingView CDP write deadline: %w", err)
	}
	if err := connection.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set TradingView CDP read deadline: %w", err)
	}

	request := struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{
		ID:     1,
		Method: "Runtime.evaluate",
		Params: map[string]any{
			"expression":    tradingViewSetSymbolExpression(symbol),
			"awaitPromise":  true,
			"returnByValue": true,
		},
	}
	if err := connection.WriteJSON(request); err != nil {
		return fmt.Errorf("send TradingView CDP command: %w", err)
	}

	for {
		var message struct {
			ID    int `json:"id"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Result *struct {
				Result *struct {
					Value struct {
						Current string `json:"current"`
					} `json:"value"`
				} `json:"result,omitempty"`
				ExceptionDetails *struct {
					Text      string `json:"text"`
					Exception *struct {
						Description string `json:"description"`
					} `json:"exception,omitempty"`
				} `json:"exceptionDetails,omitempty"`
			} `json:"result,omitempty"`
		}
		if err := connection.ReadJSON(&message); err != nil {
			return fmt.Errorf("read TradingView CDP response: %w", err)
		}
		// CDP events have no matching request ID. Ignore them until the
		// Runtime.evaluate response arrives.
		if message.ID != request.ID {
			continue
		}
		if message.Error != nil {
			return fmt.Errorf("TradingView CDP error %d: %s", message.Error.Code, message.Error.Message)
		}
		if message.Result != nil && message.Result.ExceptionDetails != nil {
			text := strings.TrimSpace(message.Result.ExceptionDetails.Text)
			if exception := message.Result.ExceptionDetails.Exception; exception != nil && strings.TrimSpace(exception.Description) != "" {
				text = strings.TrimSpace(exception.Description)
			}
			if text == "" {
				text = "TradingView rejected the symbol change"
			}
			return errors.New(text)
		}
		if message.Result != nil && message.Result.Result != nil {
			current := message.Result.Result.Value.Current
			if current != "" && !tradingViewSymbolMatches(current, symbol) {
				return fmt.Errorf("TradingView reported %q after requesting %q", current, symbol)
			}
		}
		return nil
	}
}

func (c *tradingViewClient) findChartTarget(ctx context.Context) (tradingViewCDPTarget, error) {
	endpoint := *c.cdpURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/json/list"
	endpoint.RawQuery = ""
	endpoint.Fragment = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return tradingViewCDPTarget{}, fmt.Errorf("prepare TradingView target request: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return tradingViewCDPTarget{}, fmt.Errorf("query TradingView debug port: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return tradingViewCDPTarget{}, fmt.Errorf("TradingView debug port returned %s", message)
	}

	var targets []tradingViewCDPTarget
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxTradingViewCDPBody))
	if err := decoder.Decode(&targets); err != nil {
		return tradingViewCDPTarget{}, fmt.Errorf("decode TradingView targets: %w", err)
	}
	if target, ok := selectTradingViewChartTarget(targets); ok {
		return target, nil
	}
	return tradingViewCDPTarget{}, errors.New("no TradingView chart target found")
}

func selectTradingViewChartTarget(targets []tradingViewCDPTarget) (tradingViewCDPTarget, bool) {
	var fallback tradingViewCDPTarget
	for _, target := range targets {
		if target.Type != "page" || !validLoopbackWebSocketURL(target.WebSocketDebuggerURL) {
			continue
		}
		urlText := strings.ToLower(target.URL)
		if strings.Contains(urlText, "tradingview.com/chart") {
			return target, true
		}
		text := strings.ToLower(target.URL + " " + target.Title)
		if fallback.WebSocketDebuggerURL == "" && strings.Contains(text, "tradingview") {
			fallback = target
		}
	}
	return fallback, fallback.WebSocketDebuggerURL != ""
}

func tradingViewSetSymbolExpression(symbol string) string {
	quoted, _ := json.Marshal(symbol)
	literal := string(quoted)
	return `(function() {
  var api = window.TradingViewApi;
  if (!api || !api._activeChartWidgetWV || typeof api._activeChartWidgetWV.value !== 'function') {
    throw new Error('TradingView chart API is unavailable');
  }
  var chart = api._activeChartWidgetWV.value();
  if (!chart || typeof chart.setSymbol !== 'function') {
    throw new Error('TradingView active chart is unavailable');
  }
  return new Promise(function(resolve) {
    chart.setSymbol(` + literal + `, {});
    setTimeout(function() {
      var current = '';
      try {
        if (typeof chart.symbol === 'function') current = chart.symbol();
      } catch (_) {}
      resolve({ requested: ` + literal + `, current: current });
    }, 500);
  });
})()`
}

func tradingViewSymbolMatches(current, requested string) bool {
	current = strings.ToUpper(strings.TrimSpace(current))
	requested = strings.ToUpper(strings.TrimSpace(requested))
	return current == requested || strings.HasSuffix(current, ":"+requested)
}

func tradingViewStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requestIsLoopback(r) {
		http.Error(w, "TradingView status is limited to local requests", http.StatusForbidden)
		return
	}
	client := tradingViewClientForRequest()
	if client == nil {
		writeTradingViewJSON(w, http.StatusOK, tradingViewStatus{})
		return
	}
	writeTradingViewJSON(w, http.StatusOK, client.Status(r.Context()))
}

func tradingViewTickerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requestIsLoopback(r) {
		http.Error(w, "TradingView control is limited to local requests", http.StatusForbidden)
		return
	}
	client := tradingViewClientForRequest()
	if client == nil || !client.Enabled() {
		http.Error(w, "TradingView Desktop integration is disabled", http.StatusServiceUnavailable)
		return
	}

	defer r.Body.Close()
	var request struct {
		Symbol string `json:"symbol"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || ensureTradingViewJSONEOF(decoder) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	request.Symbol = strings.ToUpper(strings.TrimSpace(request.Symbol))
	if !tradingViewSymbolPattern.MatchString(request.Symbol) {
		http.Error(w, "invalid symbol", http.StatusBadRequest)
		return
	}
	if err := client.SetSymbol(r.Context(), request.Symbol); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeTradingViewJSON(w, http.StatusOK, map[string]string{"symbol": request.Symbol})
}

func requestIsLoopback(r *http.Request) bool {
	host := strings.TrimSpace(r.RemoteAddr)
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ensureTradingViewJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeTradingViewJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
