package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var portfolioPath = regexp.MustCompile(`^/(?:[a-z]{2}(?:-[A-Z]{2})?/)?copy-trading/lead-details/([0-9]+)$`)

//go:embed dashboard.html trader.html
var pages embed.FS
var pageTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"money": func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"date":  func(v int64) string { return time.UnixMilli(v).UTC().Format("2006-01-02 15:04 UTC") },
}).ParseFS(pages, "dashboard.html", "trader.html"))

type trader struct {
	ID, Name, Start, LastError        string
	Slippage, TradingFee, ProfitShare float64
	StartingBalance                   float64
	Updated                           int64
	Balance, Profit, SmallPnL         float64
	Count, SmallCount, Wins, Losses   int
}

type trade struct {
	ID, Symbol, Side, Status string
	Opened, Closed           int64
	Margin, PnL, SourcePnL   float64
	Small                    bool
}

type symbolCount struct {
	Symbol string
	Count  int
}

type equityPoint struct {
	Closed  int64
	Balance float64
}

type view struct {
	Traders   []trader
	Selected  *trader
	Trades    []trade
	Symbols   []symbolCount
	Equity    []equityPoint
	Notice    string
	SmallOnly bool
}

type app struct {
	db     *sql.DB
	client *http.Client
	base   string
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "dashboard address")
	file := flag.String("db", "tracker.db", "SQLite database path")
	flag.Parse()
	db, err := sql.Open("sqlite", *file)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS traders (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', start TEXT NOT NULL, starting_balance REAL NOT NULL DEFAULT 200, slippage REAL NOT NULL DEFAULT 0.05, trading_fee REAL NOT NULL DEFAULT 0.05, profit_share REAL NOT NULL DEFAULT 10, updated INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS trades (trader_id TEXT NOT NULL, id TEXT NOT NULL, symbol TEXT NOT NULL, side TEXT NOT NULL, opened INTEGER NOT NULL, closed INTEGER NOT NULL, margin REAL NOT NULL DEFAULT 0, source_pnl REAL NOT NULL DEFAULT 0, pnl REAL NOT NULL DEFAULT 0, old INTEGER NOT NULL, small INTEGER NOT NULL, status TEXT NOT NULL, PRIMARY KEY (trader_id,id));`); err != nil {
		log.Fatal(err)
	}
	// Existing databases keep their trades and gain the new simulation settings.
	for _, column := range []struct{ name, definition string }{
		{"slippage", "REAL NOT NULL DEFAULT 0.05"},
		{"trading_fee", "REAL NOT NULL DEFAULT 0.05"},
		{"profit_share", "REAL NOT NULL DEFAULT 10"},
	} {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('traders') WHERE name=?`, column.name).Scan(&exists); err != nil {
			log.Fatal(err)
		}
		if exists == 0 {
			if _, err := db.Exec(`ALTER TABLE traders ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				log.Fatal(err)
			}
		}
	}
	a := &app{db: db, client: &http.Client{Timeout: 20 * time.Second}, base: "https://www.binance.com"}
	go a.poll(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("GET /traders/{id}", a.index)
	mux.HandleFunc("POST /traders", a.add)
	mux.HandleFunc("POST /traders/{id}/settings", a.settings)
	mux.HandleFunc("POST /traders/{id}/delete", a.deleteTrader)
	log.Printf("Dashboard: http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func parseID(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || (u.Hostname() != "www.binance.com" && u.Hostname() != "binance.com") {
		return "", errors.New("enter an https://www.binance.com copy-trading portfolio URL")
	}
	m := portfolioPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", errors.New("invalid Binance lead-details URL")
	}
	return m[1], nil
}

func parseFee(raw string) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
		return 0, errors.New("cost must be a percentage from 0 to 100")
	}
	return v, nil
}

func parseBalance(raw string) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 1000000000 {
		return 0, errors.New("starting balance must be between 0 and 1 billion")
	}
	return v, nil
}

func (a *app) add(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	id, err := parseID(r.FormValue("url"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	start := r.FormValue("start")
	d, err := time.Parse("2006-01-02", start)
	if err != nil || d.After(time.Now()) {
		http.Error(w, "enter a start date no later than today", 400)
		return
	}
	slippage, err := parseFee(r.FormValue("slippage"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fee, err := parseFee(r.FormValue("trading_fee"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	share, err := parseFee(r.FormValue("profit_share"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	amount, err := parseBalance(r.FormValue("starting_balance"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	_, err = a.db.Exec(`INSERT INTO traders(id,start,slippage,trading_fee,profit_share,starting_balance) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, id, start, slippage, fee, share, amount)
	if err != nil {
		http.Error(w, "could not save trader", 500)
		return
	}
	go a.syncNow(trader{ID: id, Start: start, Slippage: slippage, TradingFee: fee, ProfitShare: share, StartingBalance: amount})
	http.Redirect(w, r, "/traders/"+id, http.StatusSeeOther)
}

func (a *app) settings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	start := r.FormValue("start")
	d, err := time.Parse("2006-01-02", start)
	if err != nil || d.After(time.Now()) {
		http.Error(w, "invalid start date", 400)
		return
	}
	slippage, err := parseFee(r.FormValue("slippage"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fee, err := parseFee(r.FormValue("trading_fee"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	share, err := parseFee(r.FormValue("profit_share"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	amount, err := parseBalance(r.FormValue("starting_balance"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	res, err := a.db.Exec(`UPDATE traders SET start=?,slippage=?,trading_fee=?,profit_share=?,starting_balance=? WHERE id=?`, start, slippage, fee, share, amount, r.PathValue("id"))
	if err != nil {
		http.Error(w, "could not save settings", 500)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	go a.syncNow(trader{ID: r.PathValue("id"), Start: start, Slippage: slippage, TradingFee: fee, ProfitShare: share, StartingBalance: amount})
	http.Redirect(w, r, "/traders/"+r.PathValue("id"), http.StatusSeeOther)
}

func (a *app) deleteTrader(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM trades WHERE trader_id=?`, id); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	result, err := tx.Exec(`DELETE FROM traders WHERE id=?`, id)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) syncNow(t trader) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := a.sync(ctx, t); err != nil {
		log.Printf("portfolio %s: %v", t.ID, err)
		_, _ = a.db.Exec(`UPDATE traders SET error=? WHERE id=?`, err.Error(), t.ID)
	}
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT t.id,t.name,t.start,t.slippage,t.trading_fee,t.profit_share,t.starting_balance,t.updated,t.error,
		COALESCE(SUM(CASE WHEN x.closed>=unixepoch(t.start)*1000 THEN x.pnl ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN x.closed>=unixepoch(t.start)*1000 AND x.small=1 THEN x.pnl ELSE 0 END),0),
		COUNT(CASE WHEN x.closed>=unixepoch(t.start)*1000 THEN 1 END), COUNT(CASE WHEN x.closed>=unixepoch(t.start)*1000 AND x.small=1 THEN 1 END),
		COUNT(CASE WHEN x.closed>=unixepoch(t.start)*1000 AND x.pnl>0 THEN 1 END), COUNT(CASE WHEN x.closed>=unixepoch(t.start)*1000 AND x.pnl<0 THEN 1 END)
		FROM traders t LEFT JOIN trades x ON x.trader_id=t.id GROUP BY t.id ORDER BY t.id`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	v := view{Notice: "Public Binance history exposes closed futures positions only. Open positions and unrealized P&L are unavailable; balances are closed-trade estimates, not live wallet equity."}
	for rows.Next() {
		var t trader
		if err = rows.Scan(&t.ID, &t.Name, &t.Start, &t.Slippage, &t.TradingFee, &t.ProfitShare, &t.StartingBalance, &t.Updated, &t.LastError, &t.Profit, &t.SmallPnL, &t.Count, &t.SmallCount, &t.Wins, &t.Losses); err != nil {
			break
		}
		t.Balance = t.StartingBalance + t.Profit
		v.Traders = append(v.Traders, t)
	}
	if e := rows.Err(); err == nil {
		err = e
	}
	rows.Close()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	id := r.PathValue("id")
	for i := range v.Traders {
		if v.Traders[i].ID == id {
			v.Selected = &v.Traders[i]
			break
		}
	}
	if id != "" && v.Selected == nil {
		http.NotFound(w, r)
		return
	}
	if v.Selected != nil {
		start, _ := time.Parse("2006-01-02", v.Selected.Start)
		rows, err = a.db.Query(`SELECT symbol,COUNT(*) FROM trades WHERE trader_id=? AND closed>=? GROUP BY symbol ORDER BY COUNT(*) DESC, symbol`, id, start.UnixMilli())
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		for rows.Next() {
			var s symbolCount
			if err = rows.Scan(&s.Symbol, &s.Count); err != nil {
				break
			}
			v.Symbols = append(v.Symbols, s)
		}
		if e := rows.Err(); err == nil {
			err = e
		}
		rows.Close()
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		v.SmallOnly = r.URL.Query().Get("small") == "1"
		balance := v.Selected.StartingBalance
		v.Equity = append(v.Equity, equityPoint{Closed: start.UnixMilli(), Balance: balance})
		rows, err = a.db.Query(`SELECT id,symbol,side,opened,closed,margin,source_pnl,pnl,small,status FROM trades WHERE trader_id=? AND closed>=? ORDER BY closed,id`, id, start.UnixMilli())
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		for rows.Next() {
			var x trade
			if err = rows.Scan(&x.ID, &x.Symbol, &x.Side, &x.Opened, &x.Closed, &x.Margin, &x.SourcePnL, &x.PnL, &x.Small, &x.Status); err != nil {
				break
			}
			balance += x.PnL
			v.Equity = append(v.Equity, equityPoint{Closed: x.Closed, Balance: balance})
			if !v.SmallOnly || x.Small {
				v.Trades = append(v.Trades, x)
			}
		}
		if e := rows.Err(); err == nil {
			err = e
		}
		rows.Close()
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := "dashboard.html"
	if v.Selected != nil {
		page = "trader.html"
	}
	if err := pageTemplates.ExecuteTemplate(w, page, v); err != nil {
		log.Print(err)
	}
}

type envelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}
type detail struct {
	Nickname      string `json:"nickname"`
	MarginBalance string `json:"marginBalance"`
}
type history struct {
	List  []position `json:"list"`
	Total int        `json:"total"`
}
type position struct {
	ID       string  `json:"positionId"`
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Opened   int64   `json:"opened"`
	Closed   int64   `json:"closed"`
	AvgCost  float64 `json:"avgCost"`
	AvgClose float64 `json:"avgClosePrice"`
	Volume   float64 `json:"closedVolume"`
	Leverage string  `json:"leverage"`
	PnL      float64 `json:"closingPnl"`
	Status   string  `json:"status"`
}

func (a *app) fetch(ctx context.Context, method, path string, body any, out any) error {
	var content io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		content = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, content)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; portfolio-tracker/1.0)")
	req.Header.Set("Origin", "https://www.binance.com")
	req.Header.Set("Referer", "https://www.binance.com/")
	req.Header.Set("clienttype", "web")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Binance HTTP %d", resp.StatusCode)
	}
	var env envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&env); err != nil {
		return err
	}
	if env.Code != "000000" {
		return fmt.Errorf("Binance code %s: %s", env.Code, env.Message)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return errors.New("portfolio data unavailable")
	}
	return json.Unmarshal(env.Data, out)
}

func (a *app) poll(ctx context.Context) {
	for {
		rows, err := a.db.Query(`SELECT id,start,slippage,trading_fee,profit_share,starting_balance FROM traders`)
		if err != nil {
			log.Print(err)
		} else {
			var traders []trader
			for rows.Next() {
				var t trader
				if err := rows.Scan(&t.ID, &t.Start, &t.Slippage, &t.TradingFee, &t.ProfitShare, &t.StartingBalance); err == nil {
					traders = append(traders, t)
				}
			}
			rows.Close()
			for _, t := range traders {
				if err := a.sync(ctx, t); err != nil {
					log.Printf("portfolio %s: %v", t.ID, err)
					_, _ = a.db.Exec(`UPDATE traders SET error=? WHERE id=?`, err.Error(), t.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

func (a *app) sync(ctx context.Context, t trader) error {
	var d detail
	if err := a.fetch(ctx, "GET", "/bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/detail?portfolioId="+t.ID, nil, &d); err != nil {
		return err
	}
	balance, err := strconv.ParseFloat(d.MarginBalance, 64)
	if err != nil || balance <= 0 || math.IsInf(balance, 0) || math.IsNaN(balance) {
		return errors.New("portfolio margin balance unavailable; cannot calculate allocation")
	}
	var all []position
	for page := 1; page <= 40; page++ {
		var h history
		err := a.fetch(ctx, "POST", "/bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/position-history", map[string]any{"portfolioId": t.ID, "pageNumber": page, "pageSize": 50}, &h)
		if err != nil {
			return err
		}
		all = append(all, h.List...)
		if len(h.List) == 0 || len(all) >= h.Total {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM traders WHERE id=?`, t.ID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	for _, p := range all {
		if p.ID == "" || p.Opened <= 0 || p.Closed <= 0 || p.Volume <= 0 || p.AvgCost <= 0 || p.AvgClose <= 0 || (!strings.EqualFold(p.Side, "Long") && !strings.EqualFold(p.Side, "Short")) {
			continue
		}
		leverage, err := strconv.ParseFloat(p.Leverage, 64)
		if err != nil || leverage <= 0 {
			continue
		}
		margin := p.Volume * p.AvgCost / leverage
		if margin <= 0 {
			continue
		}
		allocated := t.StartingBalance * margin / balance
		if allocated > t.StartingBalance {
			allocated = t.StartingBalance
		}
		if allocated <= 0 || math.IsNaN(allocated) || math.IsInf(allocated, 0) {
			continue
		}
		small := allocated < 10
		pnl := simulatedPnL(p, allocated, leverage, t)
		// Keep the legacy old column for existing databases; inclusion now depends on close time.
		_, err = tx.Exec(`INSERT INTO trades(trader_id,id,symbol,side,opened,closed,margin,source_pnl,pnl,old,small,status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(trader_id,id) DO UPDATE SET symbol=excluded.symbol,side=excluded.side,opened=excluded.opened,closed=excluded.closed,margin=excluded.margin,source_pnl=excluded.source_pnl,pnl=excluded.pnl,old=excluded.old,small=excluded.small,status=excluded.status`,
			t.ID, p.ID, p.Symbol, p.Side, p.Opened, p.Closed, allocated, p.PnL, pnl, 0, small, p.Status)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE traders SET name=?,updated=?,error='' WHERE id=?`, d.Nickname, time.Now().UnixMilli(), t.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func simulatedPnL(p position, allocated, leverage float64, t trader) float64 {
	quantity := allocated * leverage / p.AvgCost
	entry := p.AvgCost * (1 + t.Slippage/100)
	exit := p.AvgClose * (1 - t.Slippage/100)
	if strings.EqualFold(p.Side, "Short") {
		entry = p.AvgCost * (1 - t.Slippage/100)
		exit = p.AvgClose * (1 + t.Slippage/100)
	}
	gross := (exit - entry) * quantity
	if strings.EqualFold(p.Side, "Short") {
		gross = -gross
	}
	pnl := gross - (entry+exit)*quantity*t.TradingFee/100
	if pnl > 0 {
		pnl *= 1 - t.ProfitShare/100
	}
	return pnl
}

// shortcut: the public history contains futures only and has no historical balance or open positions; use current margin balance as the allocation denominator until an authenticated historical feed exists.
