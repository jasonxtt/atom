package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const (
	idleLimit      = 30 * time.Minute
	afterDoneLimit = 5 * time.Minute
)

// Server is the wizard: a page and a small JSON API. It only answers private and loopback addresses.
type Server struct {
	ins  *Installer
	mu   sync.Mutex
	job  *Job
	seen time.Time
	quit chan struct{}
	once sync.Once
}

func newServer(ins *Installer) *Server {
	return &Server{ins: ins, seen: time.Now(), quit: make(chan struct{})}
}

func isPrivateRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// hostAllowed refuses a Host header that is not an address, localhost or this machine's own name: a web page
// that makes the browser reach the wizard through a name of its own (DNS rebinding) is not the user.
func hostAllowed(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if net.ParseIP(h) != nil || strings.EqualFold(h, "localhost") {
		return true
	}
	name, _ := os.Hostname()
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	return name != "" && (h == name || h == name+".local" || h == name+".lan")
}

// sameOriginRequest keeps other web pages from driving the wizard through the user's browser: a request that
// changes something must be JSON (which a cross-site page cannot send without a preflight the server never
// grants) and must not name another origin.
func sameOriginRequest(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/json"
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeResp(w, 200, s.state()) })
	mux.HandleFunc("POST /api/free", s.handleFree)
	mux.HandleFunc("POST /api/validate", s.handleValidate)
	mux.HandleFunc("POST /api/install", s.handleInstall)
	mux.HandleFunc("GET /api/job", s.handleJob)
	mux.HandleFunc("POST /api/finish", s.handleFinish)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPrivateRemote(r.RemoteAddr) || !hostAllowed(r.Host) || !sameOriginRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.mu.Lock()
		s.seen = time.Now()
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

func writeResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeResp(w, code, map[string]string{"error": msg})
}

type defaults struct {
	Domestic    string `json:"domestic"`
	Foreign     string `json:"foreign"`
	FakeIPRange string `json:"fakeip_range"`
	MixedPort   int    `json:"mixed_port"`
	PanelPort   int    `json:"panel_port"`
}

type stateResp struct {
	Version       string     `json:"version"`
	System        SystemInfo `json:"system"`
	Ifaces        []Iface    `json:"ifaces"`
	Addrs         []string   `json:"addrs"`
	Port53        []Occupant `json:"port53"`
	ScanError     string     `json:"scan_error,omitempty"`
	Defaults      defaults   `json:"defaults"`
	InstallerPort int        `json:"installer_port"`
	Installed     *struct {
		Version   string `json:"version"`
		PanelPort int    `json:"panel_port"`
	} `json:"installed,omitempty"`
	Running bool `json:"running"`
	HasData bool `json:"has_data"`
}

func (s *Server) state() stateResp {
	r := stateResp{Version: s.ins.Version, System: checkSystem(), Ifaces: listInterfaces(), Addrs: privateAddrs(), InstallerPort: s.ins.InstallerPort,
		Defaults: defaults{defaultDomestic, defaultForeign, defaultFakeIP, defaultMixed, defaultPanel}}
	if r.Ifaces == nil {
		r.Ifaces = []Iface{}
	}
	if r.Addrs == nil {
		r.Addrs = []string{}
	}
	if occ, err := portOccupants(53); err != nil {
		r.ScanError = err.Error()
		r.System.Problems = append(r.System.Problems, "需要 ss 命令（iproute2）来检查端口")
	} else {
		r.Port53 = occ
	}
	if r.Port53 == nil {
		r.Port53 = []Occupant{}
	}
	if st, err := loadState(s.ins.Paths); err == nil && st.Complete {
		r.Installed = &struct {
			Version   string `json:"version"`
			PanelPort int    `json:"panel_port"`
		}{st.Version, st.PanelPort}
	}
	r.HasData = hasData(s.ins.Paths)
	s.mu.Lock()
	r.Running = s.job != nil && !s.job.view(0).Done
	s.mu.Unlock()
	return r
}

func (s *Server) handleFree(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
		Unit string `json:"unit"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	occ, err := portOccupants(53)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var target *Occupant
	for i := range occ {
		if occ[i].Kind == req.Kind && (req.Kind == "resolved" || occ[i].Unit == req.Unit) {
			target = &occ[i]
		}
	}
	if target == nil {
		writeErr(w, 409, "53 端口上已经没有这个占用者")
		return
	}
	if req.Kind == "resolved" {
		err = freeResolvedStub(s.ins.Paths, stderrLogger{})
	} else {
		err = freeUnit(s.ins.Paths, target.Unit, stderrLogger{})
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	time.Sleep(time.Second) // let the listeners go away before the next scan
	writeResp(w, 200, s.state())
}

func (s *Server) busy() func(port int) string {
	ls, err := listeners()
	if err != nil {
		return func(int) string { return "" }
	}
	return func(port int) string {
		if occ := occupantsOf(ls, port, unitOfPID); len(occ) > 0 {
			return occ[0].Proc
		}
		return ""
	}
}

func (c *Config) fillDefaults() {
	if strings.TrimSpace(c.Domestic) == "" {
		c.Domestic = defaultDomestic
	}
	if strings.TrimSpace(c.Foreign) == "" {
		c.Foreign = defaultForeign
	}
	if strings.TrimSpace(c.FakeIPRange) == "" {
		c.FakeIPRange = defaultFakeIP
	}
	c.FakeIPRange = strings.TrimSpace(c.FakeIPRange)
	c.Topology = strings.TrimSpace(c.Topology)
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	cfg.fillDefaults()
	cfg.keepData = hasData(s.ins.Paths)
	errs := cfg.Validate(listInterfaces(), s.ins.InstallerPort, s.busy())
	if errs == nil {
		errs = []FieldError{}
	}
	writeResp(w, 200, map[string]any{"errors": errs})
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	cfg.fillDefaults()
	cfg.keepData = hasData(s.ins.Paths)
	if probs := checkSystem().Problems; len(probs) > 0 {
		writeErr(w, 400, strings.Join(probs, "；"))
		return
	}
	if occ, err := portOccupants(53); err != nil || len(occ) > 0 {
		writeErr(w, 409, "53 端口被占用")
		return
	}
	if errs := cfg.Validate(listInterfaces(), s.ins.InstallerPort, s.busy()); len(errs) > 0 {
		writeResp(w, 400, map[string]any{"error": "配置不对", "errors": errs})
		return
	}
	s.mu.Lock()
	if s.job != nil && !s.job.view(0).Done {
		s.mu.Unlock()
		writeErr(w, 409, "正在安装")
		return
	}
	job := newJob(installSteps)
	s.job = job
	s.mu.Unlock()
	go s.ins.runInstall(cfg, job)
	writeResp(w, 202, map[string]bool{"started": true})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	job := s.job
	s.mu.Unlock()
	if job == nil {
		writeResp(w, 200, map[string]bool{"idle": true})
		return
	}
	since := 0
	fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)
	writeResp(w, 200, job.view(since))
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.job != nil && !s.job.view(0).Done
	s.mu.Unlock()
	if running {
		writeErr(w, 409, "正在安装")
		return
	}
	writeResp(w, 200, map[string]bool{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.stop(true)
	}()
}

// stop ends the wizard; after a successful installation it also removes the unpacked package.
func (s *Server) stop(cleanup bool) {
	s.once.Do(func() {
		if cleanup {
			s.mu.Lock()
			ok := s.job != nil && s.job.view(0).OK
			s.mu.Unlock()
			if ok {
				os.RemoveAll(s.ins.Paths.Pkg())
			}
		}
		close(s.quit)
	})
}

// watchIdle ends the wizard when nobody uses it any more.
func (s *Server) watchIdle() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			s.mu.Lock()
			idle := time.Since(s.seen)
			var done, ok bool
			if s.job != nil {
				v := s.job.view(0)
				done, ok = v.Done, v.OK
			}
			running := s.job != nil && !done
			s.mu.Unlock()
			switch {
			case running:
			case done && ok && idle > afterDoneLimit, idle > idleLimit:
				s.stop(done && ok)
			}
		}
	}
}
