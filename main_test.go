package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseID(t *testing.T) {
	id, err := parseID("https://www.binance.com/en/copy-trading/lead-details/5317262875933053185?timeRange=30D")
	if err != nil || id != "5317262875933053185" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := parseID("https://example.com/en/copy-trading/lead-details/5317262875933053185"); err == nil {
		t.Fatal("accepted untrusted domain")
	}
}

func TestSyncBackfillAndStartingBalance(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE trades (trader_id TEXT,id TEXT,symbol TEXT,side TEXT,opened INTEGER,closed INTEGER,margin REAL,source_pnl REAL,pnl REAL,old INTEGER,small INTEGER,status TEXT,PRIMARY KEY(trader_id,id)); CREATE TABLE traders (id TEXT PRIMARY KEY,name TEXT,updated INTEGER,error TEXT); INSERT INTO traders(id) VALUES('42');`)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse("2006-01-02", "2026-11-01")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			fmt.Fprint(w, `{"code":"000000","data":{"nickname":"Example","marginBalance":"1000"}}`)
			return
		}
		fmt.Fprintf(w, `{"code":"000000","data":{"total":2,"list":[{"positionId":"old","symbol":"BTCUSDT","side":"Long","opened":%d,"closed":%d,"avgCost":100,"avgClosePrice":105,"closedVolume":1,"leverage":"10","closingPnl":5},{"positionId":"new","symbol":"ETHUSDT","side":"Short","opened":%d,"closed":%d,"avgCost":100,"avgClosePrice":95,"closedVolume":1,"leverage":"10","closingPnl":5}]}}`, start.Add(-time.Hour).UnixMilli(), start.Add(time.Hour).UnixMilli(), start.Add(time.Hour).UnixMilli(), start.Add(2*time.Hour).UnixMilli())
	}))
	defer server.Close()
	a := &app{db: db, client: server.Client(), base: server.URL}
	tr := trader{ID: "42", Start: "2026-11-01", StartingBalance: 200}
	if err := a.sync(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	var margin, pnl float64
	var old, small int
	if err := db.QueryRow(`SELECT margin,pnl,old,small FROM trades WHERE id='new'`).Scan(&margin, &pnl, &old, &small); err != nil {
		t.Fatal(err)
	}
	if margin != 2 || pnl != 1 || old != 0 || small != 1 {
		t.Fatalf("new: margin=%v pnl=%v old=%d small=%d", margin, pnl, old, small)
	}
	if err := db.QueryRow(`SELECT old FROM trades WHERE id='old'`).Scan(&old); err != nil || old != 1 {
		t.Fatalf("old=%d err=%v", old, err)
	}
	tr.StartingBalance = 1000
	if err := a.sync(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT margin,pnl,small FROM trades WHERE id='new'`).Scan(&margin, &pnl, &small); err != nil {
		t.Fatal(err)
	}
	if margin != 10 || pnl != 5 || small != 0 {
		t.Fatalf("resized: margin=%v pnl=%v small=%d", margin, pnl, small)
	}
}

func TestSimulatedCostsAndProfitShare(t *testing.T) {
	tr := trader{Slippage: 0.05, TradingFee: 0.05, ProfitShare: 10}
	p := position{Side: "Long", AvgCost: 100, AvgClose: 110, Volume: 1}
	got := simulatedPnL(p, 10, 10, tr)
	if math.Abs(got-8.81100225) > 1e-8 {
		t.Fatalf("profit after costs and share: %v", got)
	}
	p.AvgClose = 90
	got = simulatedPnL(p, 10, 10, tr)
	if math.Abs(got-(-10.1900025)) > 1e-8 {
		t.Fatalf("loss must not pay profit share: %v", got)
	}
}
