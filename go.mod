module agentbob

go 1.26

// Run `go mod tidy` to populate require/go.sum (needs network).
// Direct deps used in this skeleton:
//   github.com/spf13/cobra      — CLI
//   gopkg.in/yaml.v3            — config
//   github.com/go-telegram/bot  — Telegram source

require (
	github.com/bwmarrin/discordgo v0.29.0
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/go-telegram/bot v1.22.0
	github.com/gorilla/websocket v1.5.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/larksuite/oapi-sdk-go/v3 v3.9.3
	golang.org/x/crypto v0.51.0
	golang.org/x/net v0.54.0
	golang.org/x/sync v0.20.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)
