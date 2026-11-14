package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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

// checkOS accepts Debian 12+, Ubuntu 22.04+ and Alpine 3.19+.
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
	case "alpine":
		_, minorStr, _ := strings.Cut(ver, ".")
		minorStr, _, _ = strings.Cut(minorStr, ".")
		minor, _ := strconv.Atoi(minorStr)
		if n < 3 || (n == 3 && minor < 19) {
			return fmt.Sprintf("需要 Alpine 3.19 或更新，当前 Alpine %s", ver)
		}
	default:
		return fmt.Sprintf("只支持 Debian、Ubuntu 和 Alpine，当前是 %s", rel["PRETTY_NAME"])
	}
	return ""
}

// inContainer: one of the marker files exists, or PID 1 has a container= variable (LXC and others).
func inContainer(markers []string, pid1Environ []byte) bool {
	for _, f := range markers {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	for _, kv := range strings.Split(string(pid1Environ), "\x00") {
		if strings.HasPrefix(kv, "container=") {
			return true
		}
	}
	return false
}

func checkArch(goarch string) string {
	if goarch != "amd64" && goarch != "arm64" {
		return "只支持 x86_64 和 aarch64"
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
	if p := checkArch(runtime.GOARCH); p != "" {
		info.Problems = append(info.Problems, p)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil && !useOpenRC() {
		info.Problems = append(info.Problems, "需要 systemd 或 OpenRC")
	}
	environ, _ := os.ReadFile("/proc/1/environ")
	if inContainer([]string{"/.dockerenv", "/run/.containerenv"}, environ) {
		info.Problems = append(info.Problems, "不支持在 Docker / LXC 容器里运行")
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
	// Kind is "resolved" for the systemd-resolved stub, "unit" (systemd) or "openrc" for another service that can be stopped
	// and disabled, "" for a process the installer cannot deal with.
	Kind string `json:"kind"`
}

func isOurUnit(unit string) bool {
	unit = rcName(unit)
	return unit == rcName(dnsUnitName) || unit == rcName(singBoxUnitName)
}

// openrcServiceOf is the OpenRC service a process belongs to: the service whose pidfile (named in
// <daemonsDir>/<service>/001) holds the process or one of its ancestors ("" if none). procDir is /proc.
func openrcServiceOf(daemonsDir, procDir string, pid int) string {
	byPID := map[int]string{}
	dirs, _ := os.ReadDir(daemonsDir)
	for _, d := range dirs {
		data, err := os.ReadFile(filepath.Join(daemonsDir, d.Name(), "001"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			pf, ok := strings.CutPrefix(line, "pidfile=")
			if !ok {
				continue
			}
			if b, err := os.ReadFile(strings.Trim(pf, `"'`)); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					byPID[n] = d.Name()
				}
			}
		}
	}
	for i := 0; pid > 1 && i < 64; i++ {
		if svc, ok := byPID[pid]; ok {
			return svc
		}
		data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
		if err != nil {
			return ""
		}
		// "pid (comm) S ppid ...": comm may hold spaces and parentheses, so cut after the last ")"
		s := string(data)
		f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
		if len(f) < 2 {
			return ""
		}
		pid, _ = strconv.Atoi(f[1])
	}
	return ""
}

// occupantsOf groups the listeners on a port by process; ours (a failed earlier try) do not count.
func occupantsOf(ls []Listener, port int, unitOf func(pid int) string, openrc bool) []Occupant {
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
		case openrc && o.Unit != "":
			// a service that others need comes back at boot with them: no button for it
			if len(openrcNeeders(o.Unit)) == 0 {
				o.Kind = "openrc"
			}
		case o.Unit == "systemd-resolved.service":
			o.Kind = "resolved"
		case o.Unit != "" && o.Unit != "atom-installer.service":
			o.Kind = "unit"
		}
		out = append(out, *o)
	}
	return out
}

// openrcNeeders lists the services that `need` svc: stopping svc stops them, and the ones in a runlevel start it
// again at boot. A variable for the tests.
var openrcNeeders = func(svc string) []string {
	var out []string
	if text, _ := quietCmd("rc-service", "-Z", svc, "stop"); text != "" { // the dry run names what would stop
		out = append(out, parseDryRunStop(text, svc)...)
	}
	if data, err := os.ReadFile("/run/openrc/deptree"); err == nil {
		for _, n := range deptreeNeedsMe(string(data), svc) {
			if len(runlevelsOf(runlevelsDir, n)) > 0 && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// parseDryRunStop reads " * stop: a b svc" of `rc-service -Z svc stop`: the other names.
func parseDryRunStop(text, svc string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		_, rest, ok := strings.Cut(line, "stop:")
		if !ok {
			continue
		}
		for _, n := range strings.Fields(rest) {
			if n != svc && !isOurUnit(n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// deptreeNeedsMe lists the services that need svc, directly, through a virtual service svc provides (dnsmasq
// provides "dns"), or through a chain of needs, from /run/openrc/deptree (depinfo_<n>_service='x',
// depinfo_<n>_needsme_<k>='y', depinfo_<n>_providedby_<k>='z').
func deptreeNeedsMe(text, svc string) []string {
	idx := map[string]string{} // service -> n
	needsme := map[string][]string{}
	providedBy := map[string][]string{} // virtual -> providers
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "depinfo_") {
			continue
		}
		v = strings.Trim(v, `'"`)
		rest := strings.TrimPrefix(k, "depinfo_")
		n, field, ok := strings.Cut(rest, "_")
		if !ok {
			continue
		}
		switch {
		case field == "service":
			idx[v] = n
		case strings.HasPrefix(field, "needsme_"):
			needsme[n] = append(needsme[n], v)
		case strings.HasPrefix(field, "providedby_"):
			providedBy[n] = append(providedBy[n], v)
		}
	}
	virtuals := map[string][]string{} // provider -> virtual services
	for name, n := range idx {
		for _, p := range providedBy[n] {
			virtuals[p] = append(virtuals[p], name)
		}
	}
	seen := map[string]bool{svc: true}
	queue := []string{svc}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next := append([]string{}, virtuals[cur]...)
		if n, ok := idx[cur]; ok {
			next = append(next, needsme[n]...)
		}
		for _, x := range next {
			if seen[x] || isOurUnit(x) {
				continue
			}
			seen[x] = true
			queue = append(queue, x)
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// serviceOfPID finds the service of a process for this machine's service manager.
func serviceOfPID() (unitOf func(pid int) string, openrc bool) {
	if useOpenRC() {
		return func(pid int) string { return openrcServiceOf("/run/openrc/daemons", "/proc", pid) }, true
	}
	return unitOfPID, false
}

func portOccupants(port int) ([]Occupant, error) {
	ls, err := listeners()
	if err != nil {
		return nil, err
	}
	unitOf, openrc := serviceOfPID()
	return occupantsOf(ls, port, unitOf, openrc), nil
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

// parseDefaultVia returns the gateway of each default route in `ip -4 route show`, by interface.
func parseDefaultVia(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "default" {
			continue
		}
		var via, dev string
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				via = f[i+1]
			case "dev":
				dev = f[i+1]
			}
		}
		if ip := net.ParseIP(via); ip != nil && ip.To4() != nil && dev != "" && out[dev] == "" {
			out[dev] = via
		}
	}
	return out
}

// parseViaRoutes picks the routes to a network through a gateway out of `ip -4 route show` (not the default
// route, not on-link ones).
func parseViaRoutes(text string) []ClientRoute {
	out := []ClientRoute{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] == "default" || !strings.Contains(f[0], "/") {
			continue
		}
		r := ClientRoute{Net: f[0]}
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				r.Via = f[i+1]
			case "dev":
				r.Dev = f[i+1]
			}
		}
		if r.Via != "" && r.Dev != "" && r.check() == nil {
			out = append(out, r)
		}
	}
	return out
}
