package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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
	var small int
	if err := db.QueryRow(`SELECT margin,pnl,small FROM trades WHERE id='new'`).Scan(&margin, &pnl, &small); err != nil {
		t.Fatal(err)
	}
	if margin != 2 || pnl != 1 || small != 1 {
		t.Fatalf("new: margin=%v pnl=%v small=%d", margin, pnl, small)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM trades WHERE id='old'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("trade opened before start not saved: count=%d err=%v", count, err)
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
	r := httptest.NewRequest("POST", "/traders/42/delete", nil)
	r.SetPathValue("id", "42")
	w := httptest.NewRecorder()
	a.deleteTrader(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("delete: status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
	if err := a.sync(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM trades`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted trades returned: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM traders`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("trader not deleted: count=%d err=%v", count, err)
	}
	w = httptest.NewRecorder()
	a.deleteTrader(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("deleting missing trader: status=%d", w.Code)
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

func TestDashboardAndTraderPages(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE traders (id TEXT, name TEXT, start TEXT, slippage REAL, trading_fee REAL, profit_share REAL, starting_balance REAL, updated INTEGER, error TEXT);
		CREATE TABLE trades (trader_id TEXT, id TEXT, symbol TEXT, side TEXT, opened INTEGER, closed INTEGER, margin REAL, source_pnl REAL, pnl REAL, old INTEGER, small INTEGER, status TEXT);
		INSERT INTO traders VALUES ('42','Example','2026-11-01',0.05,0.05,10,200,0,'');
		INSERT INTO trades VALUES
		('42','1','BTCUSDT','Long',unixepoch('2026-11-01')*1000,unixepoch('2026-11-01')*1000,5,2,1.5,0,1,'closed'),
		('42','2','BTCUSDT','Short',unixepoch('2026-11-01')*1000+1000,unixepoch('2026-11-01')*1000+2000,12,1,-2,0,0,'closed'),
		('42','3','ETHUSDT','Long',unixepoch('2026-11-01')*1000-1000,unixepoch('2026-11-01')*1000+1000,8,1,1,1,1,'closed'),
		('42','4','XRPUSDT','Long',unixepoch('2026-11-01')*1000-3000,unixepoch('2026-11-01')*1000-2000,8,1,9,1,1,'closed');`)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	for _, tc := range []struct {
		path string
		want []string
		no   []string
	}{
		{"/", []string{"Add trader", "Traders", "/traders/42", "200.50"}, []string{"symbol-chart", "id=\"trades\"", "209.50"}},
		{"/traders/42", []string{"symbol-chart", "balance-chart", "label: \"BTCUSDT\", count:  2", "label: \"ETHUSDT\", count:  1", "data-order=\"-2\"", "new DataTable", "<td class=\"fw-semibold\">ETHUSDT</td>", "200.50", "0.50", "2 / 1", "3 trades closed", "2.50", "/traders/42/delete"}, []string{"label: \"XRPUSDT\"", "<td class=\"fw-semibold\">XRPUSDT</td>", "old from totals"}},
		{"/traders/42?small=1", []string{"BTCUSDT", "balance-chart", "<td class=\"fw-semibold\">ETHUSDT</td>", "200.50"}, []string{"<td class=\"fw-semibold\">XRPUSDT</td>", "data-order=\"-2\""}},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.SetPathValue("id", strings.TrimPrefix(strings.Split(tc.path, "?")[0], "/traders/"))
		if tc.path == "/" {
			r.SetPathValue("id", "")
		}
		w := httptest.NewRecorder()
		a.index(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.path, w.Code, w.Body.String())
		}
		for _, s := range tc.want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("%s: missing %q", tc.path, s)
			}
		}
		for _, s := range tc.no {
			if strings.Contains(w.Body.String(), s) {
				t.Errorf("%s: unexpectedly contains %q", tc.path, s)
			}
		}
		if tc.path != "/" {
			series := strings.SplitN(strings.SplitN(w.Body.String(), "const equity = [", 2)[1], "];", 2)[0]
			values := regexp.MustCompile(`balance:\s*([\d.]+)`).FindAllStringSubmatch(series, -1)
			if len(values) != 4 || values[0][1] != "200" || values[1][1] != "201.5" || values[2][1] != "202.5" || values[3][1] != "200.5" {
				t.Errorf("%s: unexpected equity series: %s", tc.path, series)
			}
		}
	}
}
