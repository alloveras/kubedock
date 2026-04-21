package cmd

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/config"
	dnsconfig "github.com/joyrex2001/kubedock/internal/dnsserver/config"
	"github.com/joyrex2001/kubedock/internal/dnsserver/dns"
	"github.com/joyrex2001/kubedock/internal/dnsserver/model"
	"github.com/joyrex2001/kubedock/internal/dnsserver/support"
	"github.com/joyrex2001/kubedock/internal/dnsserver/watcher"
)

var dnsServerCmd = &cobra.Command{
	Use:   "dns-server",
	Short: "Start the kubedock DNS server for container hostname resolution",
	Long: `Start a DNS server that resolves container hostnames by reading kubedock
pod annotations (kubedock.hostalias/N, kubedock.network/N). Pods that share a
network can resolve each other's hostnames while pods on different networks are
isolated, enabling concurrent test runs in the same namespace without hostname clashes.

Use alongside 'kubedock server --disable-services --dns-server <ClusterIP>'.`,
	Run: func(cmd *cobra.Command, args []string) {
		flag.Set("v", viper.GetString("verbosity"))
		runDNSServer()
	},
}

func init() {
	rootCmd.AddCommand(dnsServerCmd)

	dnsServerCmd.PersistentFlags().String("dns-listen-addr", ":53", "DNS server listen address (e.g. :53 or :1053)")
	dnsServerCmd.PersistentFlags().StringP("namespace", "n", getContextNamespace(), "Namespace to watch for kubedock pods")
	dnsServerCmd.PersistentFlags().StringSlice("internal-domain", []string{}, "Additional domains treated as internal (not forwarded to upstream when missing)")
	dnsServerCmd.PersistentFlags().StringP("verbosity", "v", "1", "Log verbosity level")

	viper.BindPFlag("dnsserver.listen-addr", dnsServerCmd.PersistentFlags().Lookup("dns-listen-addr"))
	viper.BindPFlag("kubernetes.namespace", dnsServerCmd.PersistentFlags().Lookup("namespace"))
	viper.BindPFlag("dnsserver.internal-domains", dnsServerCmd.PersistentFlags().Lookup("internal-domain"))
	viper.BindPFlag("verbosity", dnsServerCmd.PersistentFlags().Lookup("verbosity"))

	viper.BindEnv("dnsserver.listen-addr", "DNS_LISTEN_ADDR")
	viper.BindEnv("kubernetes.namespace", "NAMESPACE")
	viper.BindEnv("verbosity", "VERBOSITY")

	if home := homeDir(); home != "" {
		dnsServerCmd.PersistentFlags().String("kubeconfig", homeDir()+"/.kube/config", "(optional) absolute path to the kubeconfig file")
	} else {
		dnsServerCmd.PersistentFlags().String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	viper.BindPFlag("kubernetes.kubeconfig", dnsServerCmd.PersistentFlags().Lookup("kubeconfig"))
}

// dnsWatcherIntegration wires the pod watcher to the DNS server: when the set
// of pods changes, it rebuilds the Networks snapshot and pushes it to the DNS server.
type dnsWatcherIntegration struct {
	pods      *model.Pods
	dnsServer *dns.KubeDockDns
}

func (d *dnsWatcherIntegration) AddOrUpdate(pod *model.Pod) {
	klog.V(2).Infof("%s/%s: pod added or updated", pod.Namespace, pod.Name)
	if d.pods.AddOrUpdate(pod) {
		d.rebuildNetworks()
	}
}

func (d *dnsWatcherIntegration) Delete(namespace, name string) {
	klog.V(2).Infof("%s/%s: pod deleted", namespace, name)
	d.pods.Delete(namespace, name)
	d.rebuildNetworks()
}

func (d *dnsWatcherIntegration) rebuildNetworks() {
	networks, err := d.pods.Networks()
	if err != nil {
		klog.Warningf("Errors building network configuration (only conflicting pods are affected): %v", err)
	}
	d.dnsServer.SetNetworks(networks)
	if klog.V(3) {
		networks.Log()
	}
}

func runDNSServer() {
	namespace := viper.GetString("kubernetes.namespace")
	listenAddr := viper.GetString("dnsserver.listen-addr")
	internalDomains := viper.GetStringSlice("dnsserver.internal-domains")

	klog.Infof("Starting kubedock DNS server (namespace=%s, addr=%s)", namespace, listenAddr)

	restConfig, err := config.GetKubernetes()
	if err != nil {
		klog.Fatalf("Failed to get kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create kubernetes client: %v", err)
	}

	clientConfig := support.GetClientConfig()
	searchDomain := ""
	if len(clientConfig.Search) > 0 {
		searchDomain = clientConfig.Search[0]
	}
	upstreamAddr := "127.0.0.1:53"
	if len(clientConfig.Servers) > 0 {
		upstreamAddr = clientConfig.Servers[0] + ":53"
	}

	upstream := dns.NewExternalDNSServer(upstreamAddr)
	klog.Infof("Upstream DNS server: %s", upstreamAddr)

	dnsServer := dns.NewKubeDockDns(upstream, listenAddr, searchDomain, internalDomains)

	// Allow overriding source IP for testing (mirrors original kubedock-dns behaviour).
	if sourceIP := os.Getenv("KUBEDOCK_DNS_SOURCE_IP"); sourceIP != "" {
		dnsServer.OverrideSourceIP(model.IPAddress(sourceIP))
	}

	podAdmin := &dnsWatcherIntegration{
		pods:      model.NewPods(),
		dnsServer: dnsServer,
	}

	podConfig := dnsconfig.PodConfig{
		HostAliasPrefix: config.HostAliasAnnotationPrefix,
		NetworkIdPrefix: config.NetworkAnnotationPrefix,
		LabelName:       config.KubedockPodLabel,
	}

	stop := make(chan struct{})

	go func() {
		dnsServer.Serve()
	}()

	go watcher.WatchPods(clientset, namespace, podAdmin, podConfig, stop)

	// Block until SIGTERM or SIGINT.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	klog.Infof("Received signal %v, shutting down", sig)
	close(stop)
}
