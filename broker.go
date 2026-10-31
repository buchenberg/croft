package croft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// brokerHosts is the exact-host allowlist for a broker network posture. An
// empty set is a deny-all broker: the proxy still exists and still pins the
// container to one host port, but it admits no destination.
type brokerHosts map[string]bool

func newBrokerHosts(hosts []string) brokerHosts {
	out := make(brokerHosts, len(hosts))
	for _, host := range hosts {
		out[strings.ToLower(strings.TrimSpace(host))] = true
	}
	return out
}

func brokerTargetHost(hostPort string) string {
	host := strings.TrimSpace(hostPort)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

func (h brokerHosts) allows(hostPort string) bool {
	return h[brokerTargetHost(hostPort)]
}

// brokerProxy is the host-side HTTP/CONNECT allowlist proxy. It listens on the
// host end of the container's veth, so the container has no route except the
// proxy address; the allowlist is the only path to the outside.
type brokerProxy struct {
	hosts brokerHosts
	ln    net.Listener
	srv   *http.Server
	once  sync.Once

	client *http.Client
}

func newBrokerProxy(hosts []string) (*brokerProxy, error) {
	p := &brokerProxy{
		hosts: newBrokerHosts(hosts),
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 nil,
				ForceAttemptHTTP2:     false,
				MaxIdleConns:          16,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return p, nil
}

func (p *brokerProxy) serve(ln net.Listener) {
	p.once.Do(func() {
		p.ln = ln
		p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			_ = p.srv.Serve(ln)
		}()
	})
}

func (p *brokerProxy) close() {
	if p.srv != nil {
		_ = p.srv.Close()
	}
	if p.ln != nil {
		_ = p.ln.Close()
	}
}

func (p *brokerProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	if !p.hosts.allows(target) || !p.hosts.allows(r.Host) {
		http.Error(w, "broker: host not allowed", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Proxy-Authorization")
	if out.URL.Scheme == "" {
		out.URL.Scheme = "http"
	}
	if out.URL.Host == "" {
		out.URL.Host = r.Host
	}
	out.Header.Del("Connection")
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Keep-Alive")
	out.Header.Del("Proxy-Authenticate")
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Te")
	out.Header.Del("Trailer")
	out.Header.Del("Transfer-Encoding")
	out.Header.Del("Upgrade")
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "broker: upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	hop := map[string]bool{
		"connection": true, "proxy-connection": true, "keep-alive": true,
		"proxy-authenticate": true, "proxy-authorization": true,
		"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
	}
	for _, value := range resp.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			hop[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	for key, values := range resp.Header {
		if hop[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *brokerProxy) connect(w http.ResponseWriter, r *http.Request) {
	if !p.hosts.allows(r.Host) {
		http.Error(w, "broker: host not allowed", http.StatusForbidden)
		return
	}
	upstream, err := net.DialTimeout("tcp", r.Host, 30*time.Second)
	if err != nil {
		http.Error(w, "broker: upstream connect failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "broker: hijack unavailable", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}

type brokerState struct {
	id  string
	net *brokerNet
}

// brokerNet is the per-generation veth + proxy + firewall setup.
type brokerNet struct {
	hostIf        string
	chain         string
	hostIP        string
	port          int
	addrIndex     uint32
	proxy         *brokerProxy
	linkCreated   bool
	chainCreated  bool
	ruleInstalled bool
	closeOnce     sync.Once
}

func (b *brokerNet) env() map[string]string {
	proxy := "http://" + net.JoinHostPort(b.hostIP, strconv.Itoa(b.port))
	return map[string]string{
		"HTTP_PROXY":  proxy,
		"HTTPS_PROXY": proxy,
		"NO_PROXY":    "127.0.0.1,localhost,::1",
	}
}

var brokerAddrs = struct {
	mu   sync.Mutex
	next uint32
	free []uint32
}{}

func allocBrokerAddr() (host, container string, index uint32, err error) {
	brokerAddrs.mu.Lock()
	defer brokerAddrs.mu.Unlock()
	if n := len(brokerAddrs.free); n > 0 {
		index = brokerAddrs.free[n-1]
		brokerAddrs.free = brokerAddrs.free[:n-1]
	} else {
		index = brokerAddrs.next
		if index >= 256*64 {
			return "", "", 0, errors.New("containerd sandbox: broker address space exhausted")
		}
		brokerAddrs.next++
	}
	hi := index / 64
	lo := (index%64)*4 + 1
	return fmt.Sprintf("10.42.%d.%d", hi, lo), fmt.Sprintf("10.42.%d.%d", hi, lo+1), index, nil
}

func releaseBrokerAddr(index uint32) {
	brokerAddrs.mu.Lock()
	brokerAddrs.free = append(brokerAddrs.free, index)
	brokerAddrs.mu.Unlock()
}

func brokerIfNames(id, snapshotKey string) (host, container string) {
	var hash uint32 = 2166136261
	for _, b := range []byte(id + "/" + snapshotKey) {
		hash ^= uint32(b)
		hash *= 16777619
	}
	return fmt.Sprintf("crh%08x", hash), fmt.Sprintf("crc%08x", hash)
}

func setupBrokerNet(ctx context.Context, id, snapshotKey string, pid uint32, hosts []string) (*brokerNet, error) {
	if pid == 0 {
		return nil, errors.New("containerd sandbox: broker network needs a running task pid")
	}
	for _, tool := range []string{"ip", "nsenter", "iptables"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("containerd sandbox: broker network requires %s: %w", tool, err)
		}
	}
	hostIP, containerIP, addrIndex, err := allocBrokerAddr()
	if err != nil {
		return nil, err
	}
	hostIf, containerIf := brokerIfNames(id, snapshotKey)
	chain := "CROFT-" + strings.TrimPrefix(hostIf, "crh")
	b := &brokerNet{hostIf: hostIf, chain: chain, hostIP: hostIP, addrIndex: addrIndex}
	cleanup := true
	defer func() {
		if cleanup {
			b.close(context.WithoutCancel(ctx))
		}
	}()

	run := func(name string, args ...string) error {
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("ip", "link", "add", hostIf, "type", "veth", "peer", "name", containerIf); err != nil {
		return nil, err
	}
	b.linkCreated = true
	if err := run("ip", "addr", "add", hostIP+"/30", "dev", hostIf); err != nil {
		return nil, err
	}
	if err := run("ip", "link", "set", hostIf, "up"); err != nil {
		return nil, err
	}
	if err := run("ip", "link", "set", containerIf, "netns", strconv.FormatUint(uint64(pid), 10)); err != nil {
		return nil, err
	}
	pidStr := strconv.FormatUint(uint64(pid), 10)
	for _, args := range [][]string{
		{"-t", pidStr, "-n", "ip", "link", "set", containerIf, "name", "eth0"},
		{"-t", pidStr, "-n", "ip", "addr", "add", containerIP + "/30", "dev", "eth0"},
		{"-t", pidStr, "-n", "ip", "link", "set", "eth0", "up"},
		{"-t", pidStr, "-n", "ip", "route", "add", hostIP + "/32", "dev", "eth0"},
	} {
		if err := run("nsenter", args...); err != nil {
			return nil, err
		}
	}

	proxy, err := newBrokerProxy(hosts)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
	if err != nil {
		return nil, fmt.Errorf("containerd sandbox: start broker proxy: %w", err)
	}
	proxy.serve(ln)
	b.proxy = proxy
	b.port = ln.Addr().(*net.TCPAddr).Port

	if err := run("iptables", "-w", "-N", chain); err != nil {
		return nil, err
	}
	b.chainCreated = true
	if err := run("iptables", "-w", "-A", chain, "-p", "tcp", "--dport", strconv.Itoa(b.port), "-j", "ACCEPT"); err != nil {
		return nil, err
	}
	if err := run("iptables", "-w", "-A", chain, "-j", "DROP"); err != nil {
		return nil, err
	}
	if err := run("iptables", "-w", "-I", "INPUT", "-i", hostIf, "-j", chain); err != nil {
		return nil, err
	}
	b.ruleInstalled = true
	cleanup = false
	return b, nil
}

func (b *brokerNet) close(ctx context.Context) {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() { b.closeOnceFn(ctx) })
}

func (b *brokerNet) closeOnceFn(ctx context.Context) {
	clean := true
	run := func(name string, args ...string) {
		if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
			clean = false
		}
	}
	if b.ruleInstalled {
		run("iptables", "-w", "-D", "INPUT", "-i", b.hostIf, "-j", b.chain)
	}
	if b.chainCreated {
		run("iptables", "-w", "-F", b.chain)
		run("iptables", "-w", "-X", b.chain)
	}
	if b.linkCreated {
		run("ip", "link", "del", b.hostIf)
	}
	if b.proxy != nil {
		b.proxy.close()
	}
	// If the host state could not be removed, keep the address reserved:
	// reusing it for a new veth while the old chain/link survives would be a
	// routing or firewall collision. A leak is the safer failure.
	if clean {
		releaseBrokerAddr(b.addrIndex)
	}
}

func mergeBrokerEnv(env map[string]string, broker map[string]string) map[string]string {
	out := make(map[string]string, len(env)+len(broker))
	for key, value := range env {
		out[key] = value
	}
	for key, value := range broker {
		out[key] = value
	}
	return out
}
