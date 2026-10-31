package croft

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrokerProxyAllowsExactHostAndRejectsOthers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "allowed")
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	proxy, err := newBrokerProxy([]string{host})
	if err != nil {
		t.Fatalf("newBrokerProxy: %v", err)
	}
	defer proxy.close()

	allowed := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, allowed)
	if rec.Code != http.StatusOK || rec.Body.String() != "allowed" {
		t.Fatalf("allowed request = %d %q, want 200 allowed", rec.Code, rec.Body.String())
	}

	denied := httptest.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, denied)
	if rec.Code != http.StatusForbidden {
		t.Errorf("denied request = %d, want 403", rec.Code)
	}
}

func TestBrokerHostsCaseInsensitiveAndPortTolerant(t *testing.T) {
	hosts := newBrokerHosts([]string{"Example.COM"})
	for _, want := range []string{"example.com", "EXAMPLE.com", "example.com:443"} {
		if !hosts.allows(want) {
			t.Errorf("hosts did not allow %q", want)
		}
	}
	if hosts.allows("other.example.com") {
		t.Error("hosts allowed a different host")
	}
}

func TestValidateBrokerHosts(t *testing.T) {
	if err := (Config{Network: "broker", BrokerHosts: []string{"api.example.com"}}).validateBrokerHosts(); err != nil {
		t.Errorf("valid broker config: %v", err)
	}
	if err := (Config{Network: "none", BrokerHosts: []string{"api.example.com"}}).validateBrokerHosts(); err == nil {
		t.Error("hosts under a non-broker network must fail")
	}
	if err := (Config{Network: "broker", BrokerHosts: []string{"bad host"}}).validateBrokerHosts(); err == nil {
		t.Error("a host containing whitespace must fail")
	}
}

func TestMergeBrokerEnvOverridesCaller(t *testing.T) {
	got := mergeBrokerEnv(map[string]string{"HTTP_PROXY": "old", "KEEP": "yes"}, map[string]string{"HTTP_PROXY": "new"})
	if got["HTTP_PROXY"] != "new" || got["KEEP"] != "yes" {
		t.Errorf("mergeBrokerEnv = %v", got)
	}
}

func TestBrokerIfNamesFitLinuxLimit(t *testing.T) {
	host, container := brokerIfNames("sandbox-abc", "croft/sandbox-abc/active/1")
	if len(host) > 15 || len(container) > 15 {
		t.Errorf("interface names %q/%q exceed IFNAMSIZ", host, container)
	}
	if host == container {
		t.Errorf("veth ends must have distinct names")
	}
}
