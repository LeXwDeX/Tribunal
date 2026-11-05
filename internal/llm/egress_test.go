package llm

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestEgressDialAddressPolicy(t *testing.T) {
	policy, err := newEgressPolicy(config.EgressConfig{DenyCIDRs: []string{"10.1.0.0/16", "169.254.10.2/32", "::1/128"}, AllowCIDRs: []string{"169.254.10.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		address string
		denied  bool
	}{
		{"169.254.169.254:80", true},
		{"169.254.10.1:80", false},
		{"169.254.10.2:80", true},
		{"[::ffff:169.254.169.254]:80", true},
		{"10.2.0.1:80", false},
		{"10.1.0.1:80", true},
		{"127.0.0.1:80", false},
		{"[::1%test]:80", true},
		{"[fe80::1]:80", true},
		{"[::]:80", true},
		{"224.0.0.1:80", true},
		{"255.255.255.255:80", true},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			err := policy.control("tcp", tt.address, syscall.RawConn(nil))
			if errors.Is(err, ErrEgressDenied) != tt.denied {
				t.Fatalf("denied = %v, want %v", errors.Is(err, ErrEgressDenied), tt.denied)
			}
		})
	}
}

func TestEgressChecksReboundDNSAtDial(t *testing.T) {
	var queries atomic.Int32
	resolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var size [2]byte
			if _, err := io.ReadFull(server, size[:]); err != nil {
				return
			}
			query := make([]byte, int(binary.BigEndian.Uint16(size[:])))
			if _, err := io.ReadFull(server, query); err != nil {
				return
			}
			end := 12
			for query[end] != 0 {
				end += int(query[end]) + 1
			}
			answer := append([]byte(nil), query[:end+5]...)
			answer[2], answer[3], answer[6], answer[7] = 0x81, 0x80, 0, 1
			answer[10], answer[11] = 0, 0
			ip := []byte{127, 0, 0, 1}
			if queries.Add(1) > 1 {
				ip = []byte{169, 254, 169, 254}
			}
			answer = append(answer, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
			answer = append(answer, ip...)
			binary.BigEndian.PutUint16(size[:], uint16(len(answer)))
			_, _ = server.Write(append(size[:], answer...))
		}()
		return client, nil
	}}
	ctx := context.Background()
	first, err := resolver.LookupIP(ctx, "ip4", "rebind.example")
	if err != nil || len(first) != 1 || !first[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("initial lookup: %v, %v", first, err)
	}
	policy, err := newEgressPolicy(config.EgressConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Resolver: resolver, Control: policy.control}
	conn, err := dialer.DialContext(ctx, "tcp4", "rebind.example:80")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("rebound dial must be denied: %v", err)
	}
}

func TestRegistryUsesEgressPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	t.Setenv("EGRESS_TEST_KEY", "test")
	registry, err := NewRegistry(map[string]config.ProviderConfig{"test": {Protocol: "openai-chat", Model: "test", BaseURL: server.URL, APIKeyEnv: "EGRESS_TEST_KEY"}}, WithEgress(config.EgressConfig{DenyCIDRs: []string{"127.0.0.0/8", "::1/128"}}))
	if err != nil {
		t.Fatal(err)
	}
	if infos := registry.Infos(); len(infos) != 1 || !infos[0].Unavailable {
		t.Fatalf("denied loopback provider was not marked unavailable: %+v", infos)
	}
	client, err := registry.Client("test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Generate(context.Background(), &Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "ping"}}}}})
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("registry request must be denied: %v", err)
	}
}

func TestRegistryStartsWithBlockedLegacyProvider(t *testing.T) {
	t.Setenv("EGRESS_TEST_KEY", "test")
	registry, err := NewRegistry(map[string]config.ProviderConfig{
		"legacy": {Protocol: "openai-chat", Model: "test", BaseURL: "http://169.254.169.254", APIKeyEnv: "EGRESS_TEST_KEY"},
	})
	if err != nil {
		t.Fatalf("blocked legacy provider must not prevent startup: %v", err)
	}
	infos := registry.Infos()
	if len(infos) != 1 || !infos[0].Unavailable || infos[0].UnavailableReason != "egress_denied" {
		t.Fatalf("blocked legacy provider was not marked unavailable: %+v", infos)
	}
	client, err := registry.Client("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if !client.Info().Unavailable || client.Info().UnavailableReason != "egress_denied" {
		t.Fatalf("client metadata is not unavailable: %+v", client.Info())
	}
	_, err = client.Generate(context.Background(), &Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "ping"}}}}})
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("legacy provider should fail with egress_denied: %v", err)
	}
}

func TestReusableEgressClientMatchesRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	t.Setenv("EGRESS_TEST_KEY", "test")
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:12345")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:12345")
	for _, tt := range []struct {
		name   string
		cfg    config.EgressConfig
		denied bool
	}{
		{"default allows loopback", config.EgressConfig{}, false},
		{"deny overrides loopback", config.EgressConfig{DenyCIDRs: []string{"127.0.0.0/8", "::1/128"}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			httpClient, err := NewEgressHTTPClient(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tr, ok := httpClient.Transport.(*http.Transport); !ok || tr.Proxy != nil {
				t.Fatal("egress client must not use an HTTP proxy")
			}
			response, err := httpClient.Get(server.URL)
			if response != nil {
				response.Body.Close()
			}
			if errors.Is(err, ErrEgressDenied) != tt.denied {
				t.Fatalf("standalone client denied = %v, error %v", errors.Is(err, ErrEgressDenied), err)
			}
			if err != nil && !tt.denied {
				t.Fatalf("allowed standalone request: %v", err)
			}
			registry, err := NewRegistry(map[string]config.ProviderConfig{"test": {Protocol: "openai-chat", Model: "test", BaseURL: server.URL, APIKeyEnv: "EGRESS_TEST_KEY"}}, WithEgress(tt.cfg))
			if err != nil {
				t.Fatal(err)
			}
			client, err := registry.Client("test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Generate(context.Background(), &Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "ping"}}}}})
			if errors.Is(err, ErrEgressDenied) != tt.denied {
				t.Fatalf("registry denied = %v, error %v", errors.Is(err, ErrEgressDenied), err)
			}
			if err != nil && !tt.denied {
				t.Fatalf("allowed registry request: %v", err)
			}
		})
	}
	if _, err := NewEgressHTTPClient(config.EgressConfig{DenyCIDRs: []string{"invalid"}}); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}

func TestEgressDeniedAtDial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	policy, err := newEgressPolicy(config.EgressConfig{DenyCIDRs: []string{"127.0.0.0/8", "::1/128"}})
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{Control: policy.control}
	client := &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
	_, err = client.Get(server.URL)
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("dial should be denied: %v", err)
	}
}

func TestRegistryHostnameRequestUsesDialControl(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	t.Setenv("EGRESS_TEST_KEY", "test")
	baseURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	registry, err := NewRegistry(map[string]config.ProviderConfig{
		"host": {Protocol: "openai-chat", Model: "test", BaseURL: baseURL, APIKeyEnv: "EGRESS_TEST_KEY"},
	}, WithEgress(config.EgressConfig{DenyCIDRs: []string{"127.0.0.0/8", "::1/128"}}))
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("host")
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Messages: []Message{{
		Role:  RoleUser,
		Parts: []Part{{Type: PartText, Text: "ping"}},
	}}}
	_, err = client.Generate(context.Background(), req)
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("hostname request must be denied by the dial control: %v", err)
	}
}
