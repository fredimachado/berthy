package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	if os.Args[1] == "serve" {
		started := time.Now()
		id, _ := os.Hostname()
		revision := os.Getenv("REVISION")
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			if revision == "bad" || (revision == "delayed" && time.Since(started) < 8*time.Second) {
				w.WriteHeader(503)
			}
		})
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			f, err := os.OpenFile("/trace/arrivals.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
			if err != nil {
				panic(err)
			}
			json.NewEncoder(f).Encode(map[string]string{"time": time.Now().UTC().Format(time.RFC3339Nano), "id": id, "revision": revision, "request": r.Header.Get("X-Request-ID")})
			f.Close()
			fmt.Fprintf(w, "%s %s\n", revision, id)
		})
		panic(http.ListenAndServe(":8080", nil))
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}}
	for i := 0; ; i++ {
		req, _ := http.NewRequest("GET", os.Args[2], nil)
		req.Header.Set("X-Request-ID", strconv.Itoa(i))
		response, err := client.Do(req)
		entry := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "request": i}
		if err != nil {
			entry["error"] = err.Error()
		} else {
			entry["status"] = response.StatusCode
			response.Body.Close()
		}
		json.NewEncoder(os.Stdout).Encode(entry)
		time.Sleep(100 * time.Millisecond)
	}
}
