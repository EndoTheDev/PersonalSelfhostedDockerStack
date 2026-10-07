// Test mock: returns 429 on the first N requests, then 200.
// For SSE requests (stream:true in body), returns SSE chunks.
// Usage: go run ./mock -fail 2 -port 11499
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	fail := flag.Int("fail", 1, "number of initial requests to fail with 429")
	port := flag.String("port", "11499", "listen port")
	flag.Parse()
	var n int32
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&n, 1)
		body, _ := io.ReadAll(r.Body)
		fmt.Printf("[mock] req #%d %s %s auth=%q stream=%v\n", cur, r.Method, r.URL.Path, r.Header.Get("Authorization"), strings.Contains(string(body), `"stream":true`))
		if int(cur) <= *fail {
			http.Error(w, `{"error":"rate limited"}`, 429)
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: {\"chunk\":%d}\n\n", i)
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(50 * time.Millisecond)
			}
			fmt.Fprintln(w, "data: [DONE]")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true,"mock":true}`)
	})
	fmt.Printf("mock listening on :%s, failing first %d requests\n", *port, *fail)
	http.ListenAndServe(":"+*port, nil)
}