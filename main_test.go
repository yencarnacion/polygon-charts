package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSymbolActionForwardsNormalizedSymbol(t *testing.T) {
	var got string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var body struct {
			Symbol string `json:"symbol"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		got = body.Symbol
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"selected":true}`))
	}))
	defer receiver.Close()

	previous := symbolActionURL
	symbolActionURL = receiver.URL
	defer func() { symbolActionURL = previous }()

	request := httptest.NewRequest(http.MethodPost, "/api/symbol-action", strings.NewReader(`{"symbol":" aapl "}`))
	response := httptest.NewRecorder()
	symbolActionHandler(response, request)
	if response.Code != http.StatusOK || got != "AAPL" || !strings.Contains(response.Body.String(), `"selected":true`) {
		t.Fatalf("code=%d symbol=%q body=%s", response.Code, got, response.Body.String())
	}
}

func TestSymbolActionRejectsInvalidSymbolAndDisabledConfig(t *testing.T) {
	previous := symbolActionURL
	defer func() { symbolActionURL = previous }()

	symbolActionURL = "http://127.0.0.1:1"
	invalid := httptest.NewRecorder()
	symbolActionHandler(invalid, httptest.NewRequest(http.MethodPost, "/api/symbol-action", strings.NewReader(`{"symbol":"bad symbol"}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid symbol code=%d", invalid.Code)
	}

	symbolActionURL = ""
	disabled := httptest.NewRecorder()
	symbolActionHandler(disabled, httptest.NewRequest(http.MethodPost, "/api/symbol-action", strings.NewReader(`{"symbol":"AAPL"}`)))
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled code=%d", disabled.Code)
	}
}
