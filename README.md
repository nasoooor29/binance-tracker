# Binance portfolio tracker

Go dashboard for tracking **public Binance lead portfolios**, backed by SQLite. Paste a lead-details URL, choose a UTC start date, and visit the dashboard. Each trader has a separate detail page with a sortable, searchable table of all trades **closed on or after the UTC start date**, a pie chart by symbol, and a closed-balance line chart starting at the configured USDT balance and moving with each simulated trade's net P&L. Both charts use all qualifying trades, even when the table is filtered to Under $10. Trades opened earlier count if they closed on or after that date. You can delete a trader and their stored trades from the detail page. The server polls Binance every 10 minutes; the dashboard refreshes every 10 seconds. Trader pages can be refreshed manually so sorting and search are not interrupted. It does **not** place trades.

```sh
go run . -addr 127.0.0.1:8080 -db tracker.db
```

Open http://127.0.0.1:8080. Keep `tracker.db` for your history. Go 1.23+ is required; `go` downloads the SQLite driver on the first run. The default binding is local-only and has no authentication; do not expose it to the internet without access controls.

The pages load Bootstrap, jQuery, DataTables, and Chart.js from public CDNs; interactive tables and charts require a connection to those CDNs.

For live-reload development run `air` in the project directory (configuration in `.air.toml`). It binds to `0.0.0.0:8080` so you can open `http://SERVER_IP:8080` from your network. **Anyone who can reach that port can edit the dashboard**; use only on a trusted network or behind access control.

## What the figures mean

- Each trader begins at **200 USDT by default**, editable on their page (also when adding them). The new-trader date defaults to **2026-11-01**. For a closed position, estimated original margin is `closedVolume × avgCost ÷ leverage`. Simulated margin is `starting balance × original margin ÷ current reported portfolio margin balance`, capped at the starting balance. Simulated gross P&L is calculated from Binance's average entry/exit prices and scaled position size, then adjusted for **slippage (default 0.05% per side), copier trading fees (default 0.05% per side), and profit sharing (default 10% of profitable simulated trades only)**. These are editable assumptions, not rates fetched from your Binance account; the profit-sharing percentage may differ by trader. Binance's reported net `closingPnl` is saved for reference but **not used** in the simulated P&L to avoid double-counting the lead trader's fees. Funding and other adjustments present in Binance's reported net P&L are not modeled. A simulated position under 10 USDT is still counted, flagged, and separately totaled.
- All trades closed on or after the chosen UTC date count in the dashboard, table, and chart, including positions opened earlier. Repeated polls upsert positions by ID, so they do not double-count. Deleting a trader permanently removes their stored trades.
- **Critical limitations:** The public lead-portfolio endpoints expose closed **futures** positions, not open positions or spot trades. Thus open-position P&L, actual wallet equity, and a true historical paper-trading replay cannot be computed. For a position already open at the chosen date, its entire simulated P&L is counted when it closes; this is not the P&L earned since the start date. The portfolio's **current**, not historical, balance is used to estimate allocation, so backfilled or later figures may change with the trader's balance. Position-history pagination and retention also limit how far back the data goes. This is an approximation, not investment advice.
