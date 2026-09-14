// Stub auth server for the live mesh e2e test — mimics a real streaming
// service: POST /login issues session cookie, GET /api is the authenticated
// account endpoint. Run: go run stub_server.go  (listens on :7801)
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["login"] != "good" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Set-Cookie", "sid=good; Path=/; HttpOnly")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sid=good") {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":{"summary":{"userCountry":"IN","subPlan":"PREMIUM","membershipStatus":"ACTIVE"}}}`)
	})

	fmt.Println("stub auth server on :7801")
	http.ListenAndServe("127.0.0.1:7801", mux)
}
