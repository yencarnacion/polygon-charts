package main

import (
	_ "embed"
	"net/http"
)

//go:embed catalyst-ui.js
var catalystUIJS string

//go:embed catalyst-ui.css
var catalystUICSS string

func catalystScriptHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(catalystUIJS))
}
func catalystStyleHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(catalystUICSS))
}
