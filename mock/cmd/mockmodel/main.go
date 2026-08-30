// Command mockmodel runs the mock OpenAI-compat model server on a fixed port,
// for manual end-to-end testing against the bob binary (point a models.yaml
// entry's base_url at http://localhost:<addr>/v1).
//
//	go run ./mock/cmd/mockmodel              # listens on :18080
//	MOCK_ADDR=:9000 go run ./mock/cmd/mockmodel
package main

import (
	"log"
	"net/http"
	"os"

	"agentbob/mock"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":18080"
	}
	// Echo the user's message so a manual tester can see their input round-trip
	// through the whole pipeline.
	h := mock.Handler(func(lastUser string) string {
		return "你好！我是本地 mock 模型。我收到了你说的：「" + lastUser + "」。整条管线是通的 ✅"
	})
	log.Printf("mock model listening on %s (OpenAI-compat: /v1/models, /v1/chat/completions)", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}
