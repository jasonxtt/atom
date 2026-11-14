package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiClient talks to the atomdns management API (/api/v1/atom).
type apiClient struct {
	base string
	http *http.Client
	// authFile is the panel login of the data directory (webinfo/atom_auth.json); while it exists every
	// request carries its key.
	authFile string
}

func newAPIClient(port int, data string) *apiClient {
	return &apiClient{
		base:     fmt.Sprintf("http://127.0.0.1:%d/api/v1/atom", port),
		http:     &http.Client{Timeout: 90 * time.Second},
		authFile: filepath.Join(data, "webinfo", "atom_auth.json"),
	}
}

// key is the key of the panel login, "" without one.
func (a *apiClient) key() string {
	data, err := os.ReadFile(a.authFile)
	if err != nil {
		return ""
	}
	var f struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(data, &f)
	return f.Key
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
		if key := a.key(); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
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

type upstreamKey struct {
	Protocol, Addr, Dial string
	Proxy                bool
}

func keysOf(us []Upstream) []upstreamKey {
	var out []upstreamKey
	for _, u := range us {
		if u.Enabled {
			out = append(out, upstreamKey{u.Protocol, u.Addr, u.DialAddr, u.UseProxy})
		}
	}
	return out
}

// sameUpstreams reports whether the group already has exactly these (enabled) upstreams.
func (a *apiClient) sameUpstreams(tag string, want []Upstream) (bool, error) {
	var resp struct {
		Groups []struct {
			Tag       string `json:"tag"`
			Upstreams []struct {
				Enabled  *bool  `json:"enabled"`
				Protocol string `json:"protocol"`
				Addr     string `json:"addr"`
				DialAddr string `json:"dial_addr"`
				UseProxy bool   `json:"use_proxy"`
			} `json:"upstreams"`
		} `json:"groups"`
	}
	if err := a.call("GET", "/upstreams", nil, &resp); err != nil {
		return false, err
	}
	for _, g := range resp.Groups {
		if g.Tag != tag {
			continue
		}
		var have []Upstream
		for _, u := range g.Upstreams {
			have = append(have, Upstream{Enabled: u.Enabled == nil || *u.Enabled, Protocol: u.Protocol, Addr: u.Addr, DialAddr: u.DialAddr, UseProxy: u.UseProxy})
		}
		hk, wk := keysOf(have), keysOf(want)
		if len(hk) != len(wk) {
			return false, nil
		}
		for i := range hk {
			if hk[i] != wk[i] {
				return false, nil
			}
		}
		return true, nil
	}
	return false, fmt.Errorf("no upstream group %s", tag)
}
