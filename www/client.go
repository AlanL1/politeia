package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/davecgh/go-spew/spew"
)

func main() {
	client := &http.Client{}

	buf := []byte(`"id": "100"`)
	r := bytes.NewReader(buf)

	req, err := http.NewRequest("POST", "http://127.0.0.1:8000", r)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("err: %v\n", err)
		return
	}
	spew.Dump(resp.Header["Set-Cookie"])
	s := strings.Split(resp.Header["Set-Cookie"][0], "=")
	spew.Dump(s)

	// lift csrf cookie
	req, err = http.NewRequest("POST", "http://127.0.0.1:8000", nil)
	req.Header.Add("X-CSRF-Token", s[1])
	req.Header = resp.Header
	resp, err = client.Do(req)
	if err != nil {
		fmt.Printf("err: %v\n", err)
		return
	}
	spew.Dump(resp.Body)
}
