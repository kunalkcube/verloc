# verloc dashboard

React SPA for the verloc gateway, served under `/dash/`. Built by Vite into
`internal/server/static/`, which is embedded in the Go binary with
`//go:embed all:static`.

Stack: React 19, Vite 8, TypeScript, Tailwind v4, Recharts, Base UI.

## Development

```sh
npm ci
npm run dev     # http://localhost:8789, proxies /dash/api to :18080
npm run build   # emits to ../internal/server/static
npm run lint    # oxlint
```

`internal/server/static/` is committed so a Go-only build works without Node.
After a frontend change, rebuild the assets **and** the Go binary; a running
binary embeds the previous assets and cannot pick up new ones from disk.
