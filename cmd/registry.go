package cmd

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/registry"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Start a CAS-backed OCI registry for kubedock test containers",
	Long: `Start a minimal OCI Distribution Spec registry that serves blob content
directly from Buildbarn's CAS via the ByteStream gRPC API.

On manifest push, all blob sizes are extracted from the manifest descriptors so
subsequent blob HEAD requests return 200 immediately — the client skips uploading
layers that are already present in CAS. Blobs are served directly from CAS at
pull time, avoiding redundant data transfer between the worker and an external
registry.`,
	Run: func(cmd *cobra.Command, args []string) {
		flag.Set("v", viper.GetString("verbosity"))
		runRegistry()
	},
}

func init() {
	rootCmd.AddCommand(registryCmd)

	registryCmd.PersistentFlags().String("registry-listen-addr", ":5000", "Registry listen address")
	registryCmd.PersistentFlags().String("tls-cert", "", "Path to TLS certificate file for serving HTTPS")
	registryCmd.PersistentFlags().String("tls-key", "", "Path to TLS private key file for serving HTTPS")
	registryCmd.PersistentFlags().String("cas-addr", "", "Buildbarn CAS gRPC address (e.g. frontend:8980)")
	registryCmd.PersistentFlags().String("cas-instance", "", "Buildbarn CAS instance name (leave empty for the default instance)")
	registryCmd.PersistentFlags().String("cas-tls-cert", "", "Path to client certificate for CAS mTLS")
	registryCmd.PersistentFlags().String("cas-tls-key", "", "Path to client key for CAS mTLS")
	registryCmd.PersistentFlags().String("cas-tls-ca", "", "Path to CA certificate for verifying the CAS server")
	registryCmd.PersistentFlags().StringP("verbosity", "v", "1", "Log verbosity level")

	viper.BindPFlag("registry.listen-addr", registryCmd.PersistentFlags().Lookup("registry-listen-addr"))
	viper.BindPFlag("registry.tls-cert", registryCmd.PersistentFlags().Lookup("tls-cert"))
	viper.BindPFlag("registry.tls-key", registryCmd.PersistentFlags().Lookup("tls-key"))
	viper.BindPFlag("registry.cas-addr", registryCmd.PersistentFlags().Lookup("cas-addr"))
	viper.BindPFlag("registry.cas-instance", registryCmd.PersistentFlags().Lookup("cas-instance"))
	viper.BindPFlag("registry.cas-tls-cert", registryCmd.PersistentFlags().Lookup("cas-tls-cert"))
	viper.BindPFlag("registry.cas-tls-key", registryCmd.PersistentFlags().Lookup("cas-tls-key"))
	viper.BindPFlag("registry.cas-tls-ca", registryCmd.PersistentFlags().Lookup("cas-tls-ca"))
	viper.BindPFlag("verbosity", registryCmd.PersistentFlags().Lookup("verbosity"))

	viper.BindEnv("registry.listen-addr", "REGISTRY_LISTEN_ADDR")
	viper.BindEnv("registry.cas-addr", "CAS_ADDR")
	viper.BindEnv("registry.cas-instance", "CAS_INSTANCE")
}

func runRegistry() {
	casAddr := viper.GetString("registry.cas-addr")
	if casAddr == "" {
		klog.Fatal("--cas-addr is required")
	}

	var (
		cas *registry.CASClient
		err error
	)
	casCert := viper.GetString("registry.cas-tls-cert")
	casKey := viper.GetString("registry.cas-tls-key")
	casCA := viper.GetString("registry.cas-tls-ca")
	if casCert != "" && casKey != "" && casCA != "" {
		cas, err = registry.NewCASClientTLS(casAddr, viper.GetString("registry.cas-instance"), casCert, casKey, casCA)
	} else {
		cas, err = registry.NewCASClient(casAddr, viper.GetString("registry.cas-instance"))
	}
	if err != nil {
		klog.Fatalf("Failed to connect to CAS: %v", err)
	}
	defer cas.Close()

	store := registry.NewStore()
	reg := registry.New(viper.GetString("registry.listen-addr"), store, cas)

	go func() {
		certFile := viper.GetString("registry.tls-cert")
		keyFile := viper.GetString("registry.tls-key")
		if err := reg.Serve(certFile, keyFile); err != nil {
			klog.Errorf("Registry stopped: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	klog.Infof("Received signal %v, shutting down", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reg.Shutdown(ctx)
}
