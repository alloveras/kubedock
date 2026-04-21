package dns

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/dnsserver/model"
)

// DNSServer is the interface for resolving DNS messages (used for the upstream server).
type DNSServer interface {
	Resolve(r *dns.Msg) *dns.Msg
}

// ExternalDNSServer forwards queries to an upstream DNS server.
type ExternalDNSServer struct {
	upstreamDNSServer string
}

func NewExternalDNSServer(upstreamDNSServer string) *ExternalDNSServer {
	return &ExternalDNSServer{upstreamDNSServer: upstreamDNSServer}
}

func (s *ExternalDNSServer) Resolve(r *dns.Msg) *dns.Msg {
	c := new(dns.Client)
	resp, _, err := c.Exchange(r, s.upstreamDNSServer)
	if err != nil {
		klog.Errorf("Error forwarding to upstream: %v", err)
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		return m
	}
	return resp
}

// KubeDockDns is a UDP DNS server that resolves container hostnames using the
// per-network pod topology maintained by the pod watcher. External names are
// proxied to an upstream DNS server.
type KubeDockDns struct {
	mutex             sync.RWMutex
	networks          *model.Networks
	upstreamDnsServer DNSServer
	port              string
	searchDomain      string
	// Internal domains are not forwarded to the upstream server when not found.
	// A SERVFAIL is returned instead, triggering client retries while the pod
	// is still starting up.
	internalDomains  []string
	overrideSourceIP model.IPAddress
}

func NewKubeDockDns(upstreamDnsServer DNSServer, port string, searchDomain string,
	internalDomains []string) *KubeDockDns {
	return &KubeDockDns{
		networks:          model.NewNetworks(),
		upstreamDnsServer: upstreamDnsServer,
		port:              port,
		searchDomain:      searchDomain,
		internalDomains:   internalDomains,
	}
}

// OverrideSourceIP fixes the source IP used for all lookups (test helper).
func (s *KubeDockDns) OverrideSourceIP(sourceIP model.IPAddress) {
	s.overrideSourceIP = sourceIP
}

// SetNetworks atomically replaces the network topology used for resolution.
func (s *KubeDockDns) SetNetworks(networks *model.Networks) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.networks = networks
}

// Serve starts the UDP DNS server and blocks until it fails.
func (s *KubeDockDns) Serve() {
	dns.HandleFunc(".", s.handleDNSRequest)
	server := &dns.Server{Addr: s.port, Net: "udp"}
	klog.Infof("Starting DNS server on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		klog.Fatalf("Failed to start DNS server: %s", err)
	}
	defer server.Shutdown()
}

func (s *KubeDockDns) isInternal(host string) bool {
	host, _ = strings.CutSuffix(host, ".")
	host, _ = strings.CutSuffix(host, "."+s.searchDomain)
	if !strings.Contains(host, ".") {
		return true
	}
	for _, domain := range s.internalDomains {
		if strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (s *KubeDockDns) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	sourceIp := s.overrideSourceIP
	if sourceIp == "" {
		raw := model.IPAddress(w.RemoteAddr().String())
		sourceIp = model.IPAddress(strings.Split(string(raw), ":")[0])
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	fallback := func() *dns.Msg { return s.upstreamDnsServer.Resolve(r) }

	// First attempt: check container topology.
	answer, err := s.answerQuestionWithNetworkSnapshot(r.Question, sourceIp, fallback)
	if err == nil {
		m.Answer = answer
		w.WriteMsg(m)
		return
	}

	// Internal hostname not found in topology on first try. Before entering the
	// 20-second container-startup retry loop, check upstream (kube-dns). If the
	// upstream resolves it, the query is for a Kubernetes service or external name
	// rather than a freshly-starting container — return immediately without waiting.
	upstreamResp := s.upstreamDnsServer.Resolve(r)
	if upstreamResp.Rcode == dns.RcodeSuccess && len(upstreamResp.Answer) > 0 {
		klog.V(3).Infof("dns: %s: %s -> upstream", sourceIp, r.Question[0].Name)
		upstreamResp.Id = r.Id
		w.WriteMsg(upstreamResp)
		return
	}

	// Neither the container topology nor upstream knows this name yet. Retry for
	// up to 20s to handle pods that do a DNS lookup before their IP is registered
	// in the watcher (e.g. freshly-started test containers).
	tend := time.Now().Add(20 * time.Second)
	for time.Now().Before(tend) {
		answer, err = s.answerQuestionWithNetworkSnapshot(r.Question, sourceIp, fallback)
		if err == nil {
			m.Answer = answer
			w.WriteMsg(m)
			return
		}
		time.Sleep(1 * time.Second)
		klog.V(2).Infof("Retrying lookup")
	}

	klog.V(3).Infof("dns: %s: %s -> SERVFAIL", sourceIp, r.Question[0].Name)
	m.Rcode = dns.RcodeServerFailure
	w.WriteMsg(m)
}

func (s *KubeDockDns) answerQuestionWithNetworkSnapshot(questions []dns.Question, sourceIp model.IPAddress, fallback func() *dns.Msg) ([]dns.RR, error) {
	s.mutex.RLock()
	networkSnapshot := s.networks
	s.mutex.RUnlock()
	return s.answerQuestion(questions, networkSnapshot, sourceIp, fallback)
}

func (s *KubeDockDns) answerQuestion(questions []dns.Question, networkSnapshot *model.Networks, sourceIp model.IPAddress, fallback func() *dns.Msg) ([]dns.RR, error) {
	answer := make([]dns.RR, 0)
	for _, question := range questions {
		var rrs []dns.RR
		internal := false
		switch question.Qtype {
		case dns.TypeA:
			internal = s.isInternal(question.Name)
			klog.V(2).Infof("dns: %s: A %s internal %v", sourceIp, question.Name, internal)
			rrs = resolveHostname(networkSnapshot, question, sourceIp, s.searchDomain)
		case dns.TypePTR:
			klog.V(2).Infof("dns: %s: PTR %s", sourceIp, question.Name)
			rrs = resolveIP(networkSnapshot, question, sourceIp)
		}
		if len(rrs) > 0 {
			answer = append(answer, rrs...)
			continue
		}
		if internal {
			return nil, fmt.Errorf("internal hostname not (yet) found")
		}
		answer = append(answer, fallback().Answer...)
	}
	return answer, nil
}

func resolveHostname(networks *model.Networks, question dns.Question, sourceIp model.IPAddress, searchDomain string) []dns.RR {
	klog.V(3).Infof("dns: %s: A %s", sourceIp, question.Name)
	hostname := question.Name[:len(question.Name)-1]
	if strings.HasSuffix(hostname, "."+searchDomain) {
		hostname = hostname[:len(hostname)-len(searchDomain)-1]
	}
	ips := networks.Lookup(sourceIp, model.Hostname(hostname))
	rrs := make([]dns.RR, 0, len(ips))
	for _, ip := range ips {
		klog.V(3).Infof("dns: %s: %s -> %s", sourceIp, question.Name, ip)
		rrs = append(rrs, createAResponse(question.Name, ip))
	}
	return rrs
}

// PTRtoIP converts a PTR name (e.g. "4.3.2.1.in-addr.arpa.") to an IP string.
func PTRtoIP(ptr string) string {
	ptr = strings.TrimSuffix(ptr, ".in-addr.arpa.")
	parts := strings.Split(ptr, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

func resolveIP(networks *model.Networks, question dns.Question, sourceIp model.IPAddress) []dns.RR {
	klog.V(3).Infof("dns: %s: PTR %s", sourceIp, question.Name)
	ip := PTRtoIP(question.Name)
	hosts := networks.ReverseLookup(sourceIp, model.IPAddress(ip))
	rrs := make([]dns.RR, 0, len(hosts))
	for _, host := range hosts {
		klog.V(3).Infof("dns: %s: %s -> %s", sourceIp, question.Name, host)
		rrs = append(rrs, createPTRResponse(question.Name, host))
	}
	return rrs
}

func createAResponse(questionName string, ip model.IPAddress) *dns.A {
	return &dns.A{
		Hdr: dns.RR_Header{
			Name:   questionName,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    300,
		},
		A: net.ParseIP(string(ip)),
	}
}

func createPTRResponse(questionName string, host model.Hostname) dns.RR {
	return &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   questionName,
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    300,
		},
		Ptr: string(host) + ".",
	}
}
