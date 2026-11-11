package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// SystemInfo says whether this machine can be installed on.
type SystemInfo struct {
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Hostname string   `json:"hostname"`
	Problems []string `json:"problems"`
}

func parseOSRelease(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok {
			out[k] = strings.Trim(v, `"'`)
		}
	}
	return out
}

// checkOS accepts Debian 12+ and Ubuntu 22.04+.
func checkOS(rel map[string]string) string {
	id, ver := rel["ID"], rel["VERSION_ID"]
	major, _, _ := strings.Cut(ver, ".")
	n, _ := strconv.Atoi(major)
	switch id {
	case "debian":
		if n < 12 {
			return fmt.Sprintf("需要 Debian 12 或更新，当前 Debian %s", ver)
		}
	case "ubuntu":
		if n < 22 {
			return fmt.Sprintf("需要 Ubuntu 22.04 或更新，当前 Ubuntu %s", ver)
		}
	default:
		return fmt.Sprintf("只支持 Debian 和 Ubuntu，当前是 %s", rel["PRETTY_NAME"])
	}
	return ""
}

func checkSystem() SystemInfo {
	info := SystemInfo{Arch: runtime.GOARCH, Problems: []string{}}
	info.Hostname, _ = os.Hostname()
	data, _ := os.ReadFile("/etc/os-release")
	rel := parseOSRelease(string(data))
	info.OS = rel["PRETTY_NAME"]
	if p := checkOS(rel); p != "" {
		info.Problems = append(info.Problems, p)
	}
	if runtime.GOARCH != "amd64" {
		info.Problems = append(info.Problems, "只支持 x86_64")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		info.Problems = append(info.Problems, "需要 systemd")
	}
	if os.Geteuid() != 0 {
		info.Problems = append(info.Problems, "需要 root 权限")
	}
	return info
}

// Iface is a network interface that can be the LAN or the WAN.
type Iface struct {
	Name    string   `json:"name"`
	Addrs   []string `json:"addrs"`
	Default bool     `json:"default"` // carries the default route
}

var virtualPrefixes = []string{"docker", "veth", "br-", "virbr", "tun", "tap", "wg", "tailscale", "zt", "cni", "flannel", "kube", "cali", "lxc", "lxd", "vnet", "ifb", "dummy", "podman"}

func isVirtualName(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseDefaultRoutes returns the interfaces with a default route from /proc/net/route.
func parseDefaultRoutes(text string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 4 && f[1] == "00000000" {
			if flags, err := strconv.ParseInt(f[3], 16, 32); err == nil && flags&2 != 0 {
				out[f[0]] = true
			}
		}
	}
	return out
}

func listInterfaces() []Iface {
	route, _ := os.ReadFile("/proc/net/route")
	def := parseDefaultRoutes(string(route))
	all, _ := net.Interfaces()
	var out []Iface
	for _, ifc := range all {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 || isVirtualName(ifc.Name) {
			continue
		}
		var v4 []string
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				v4 = append(v4, ipn.String())
			}
		}
		if len(v4) == 0 {
			continue
		}
		out = append(out, Iface{Name: ifc.Name, Addrs: v4, Default: def[ifc.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Listener is one socket in LISTEN (TCP) or UNCONN (UDP) state, from ss.
type Listener struct {
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	Proc  string `json:"proc"`
	PID   int    `json:"pid"`
}

var ssProcRe = regexp.MustCompile(`\("([^"]*)",pid=(\d+)`)

// parseSS reads `ss -H -lntupn`.
func parseSS(text string) []Listener {
	var out []Listener
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || (f[0] != "tcp" && f[0] != "udp") {
			continue
		}
		local := f[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		l := Listener{Proto: f[0], Addr: strings.TrimPrefix(strings.TrimSuffix(local[:i], "]"), "["), Port: port}
		if m := ssProcRe.FindStringSubmatch(sc.Text()); m != nil {
			l.Proc = m[1]
			l.PID, _ = strconv.Atoi(m[2])
		}
		out = append(out, l)
	}
	return out
}

func listeners() ([]Listener, error) {
	out, err := exec.Command("ss", "-H", "-lntupn").Output()
	if err != nil {
		return nil, fmt.Errorf("ss: %w", err)
	}
	return parseSS(string(out)), nil
}

var cgroupUnitRe = regexp.MustCompile(`/([^/]+\.service)(?:/|$)`)

// unitOfPID is the systemd service a process belongs to ("" if none).
func unitOfPID(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	var unit string
	for _, line := range strings.Split(string(data), "\n") {
		if m := cgroupUnitRe.FindStringSubmatch(line); m != nil {
			unit = m[1]
		}
	}
	return unit
}

// Occupant is a program listening on a port.
type Occupant struct {
	Proc  string   `json:"proc"`
	PID   int      `json:"pid"`
	Unit  string   `json:"unit"`
	Addrs []string `json:"addrs"`
	// Kind is "resolved" for the systemd-resolved stub, "unit" for another service that can be stopped
	// and disabled, "" for a process the installer cannot deal with.
	Kind string `json:"kind"`
}

func isOurUnit(unit string) bool { return unit == dnsUnitName || unit == singBoxUnitName }

// occupantsOf groups the listeners on a port by process; ours (a failed earlier try) do not count.
func occupantsOf(ls []Listener, port int, unitOf func(pid int) string) []Occupant {
	byPID := map[int]*Occupant{}
	var order []int
	for _, l := range ls {
		if l.Port != port {
			continue
		}
		o := byPID[l.PID]
		if o == nil {
			o = &Occupant{Proc: l.Proc, PID: l.PID}
			if l.PID > 0 {
				o.Unit = unitOf(l.PID)
			}
			byPID[l.PID] = o
			order = append(order, l.PID)
		}
		o.Addrs = append(o.Addrs, fmt.Sprintf("%s %s", l.Proto, l.Addr))
	}
	var out []Occupant
	for _, pid := range order {
		o := byPID[pid]
		if isOurUnit(o.Unit) {
			continue
		}
		switch {
		case o.Unit == "systemd-resolved.service":
			o.Kind = "resolved"
		case o.Unit != "" && o.Unit != "atom-installer.service":
			o.Kind = "unit"
		}
		out = append(out, *o)
	}
	return out
}

func portOccupants(port int) ([]Occupant, error) {
	ls, err := listeners()
	if err != nil {
		return nil, err
	}
	return occupantsOf(ls, port, unitOfPID), nil
}

func privateAddrs() []string {
	var out []string
	for _, ifc := range listInterfaces() {
		for _, a := range ifc.Addrs {
			ip, _, err := net.ParseCIDR(a)
			if err == nil {
				out = append(out, ip.String())
			}
		}
	}
	return out
}
