package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// apiClient talks to the atomdns management API (/api/v1/atom).
type apiClient struct {
	base string
	http *http.Client
}

func newAPIClient(port int) *apiClient {
	return &apiClient{base: fmt.Sprintf("http://127.0.0.1:%d/api/v1/atom", port), http: &http.Client{Timeout: 90 * time.Second}}
}

type apiStatus struct {
	AtomDNS struct {
		Version string `json:"version"`
	} `json:"atomdns"`
	SingBox struct {
		Running bool `json:"running"`
		InSync  bool `json:"in_sync"`
	} `json:"singbox"`
	LastApply *struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"last_apply"`
}

// call does one request. A 409 or 503 means a configuration is being published: it is retried a few times.
func (a *apiClient) call(method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	var last error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		req, err := http.NewRequest(method, a.base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			if out != nil {
				return json.Unmarshal(data, out)
			}
			return nil
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		last = fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
		if resp.StatusCode != 409 && resp.StatusCode != 503 {
			return last
		}
	}
	return last
}

func (a *apiClient) status() (*apiStatus, error) {
	var st apiStatus
	if err := a.call("GET", "/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// waitStatus polls /status until done accepts it or the time is up.
func (a *apiClient) waitStatus(timeout time.Duration, done func(*apiStatus) bool) (*apiStatus, error) {
	deadline := time.Now().Add(timeout)
	var st *apiStatus
	var err error
	for {
		st, err = a.status()
		if err == nil && done(st) {
			return st, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, err
			}
			return st, fmt.Errorf("timeout")
		}
		time.Sleep(time.Second)
	}
}
