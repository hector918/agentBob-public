module agentbob/sidecars/browser

go 1.26

// Run `go mod tidy` to populate require/go.sum (needs network).
// Direct deps used in this skeleton:
//   github.com/spf13/cobra      — CLI
//   gopkg.in/yaml.v3            — config
//   github.com/go-telegram/bot  — Telegram source

require (
	github.com/chromedp/cdproto v0.0.0-20260321001828-e3e3800016bc
	github.com/chromedp/chromedp v0.15.1
	golang.org/x/sys v0.44.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/go-json-experiment/json v0.0.0-20260214004413-d219187c3433 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	github.com/kr/pretty v0.3.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)
