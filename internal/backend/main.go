package backend

import (
	"fmt"
	"io"
	"io/fs"
	"net"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/model/types"
	"github.com/joyrex2001/kubedock/internal/util/podtemplate"
)

// Backend is the interface to orchestrate and manage kubernetes objects.
type Backend interface {
	StartContainer(*types.Container) (DeployState, error)
	StartContainerForInspection(*types.Container) (DeployState, error)
	AttachContainer(*types.Container, io.Reader, io.Writer, io.Writer, bool) error
	GetContainerStatus(*types.Container) (DeployState, error)
	CreatePortForwards(*types.Container)
	CreateReverseProxies(*types.Container)
	GetPodIP(*types.Container) (string, error)
	DeleteAll() error
	DeleteWithKubedockID(string) error
	DeleteContainer(*types.Container) error
	DeleteOlderThan(time.Duration) error
	WatchDeleteContainer(*types.Container) (chan struct{}, error)
	CopyFromContainer(*types.Container, string, io.Writer) error
	CopyToContainer(*types.Container, io.Reader, string, bool) error
	GetFileModeInContainer(tainr *types.Container, path string) (fs.FileMode, error)
	FileExistsInContainer(tainr *types.Container, path string) (bool, error)
	ExecContainer(*types.Container, *types.Exec, io.Reader, io.Writer) (int, error)
	GetLogs(*types.Container, *LogOptions, chan struct{}, io.Writer) error
	GetLogsRaw(*types.Container, *LogOptions, chan struct{}, io.Writer) error
	InspectImage(string) (godigest.Digest, ocispec.ImageConfig, error)
}

// instance is the internal representation of the Backend object.
type instance struct {
	cli               kubernetes.Interface
	cfg               *rest.Config
	podTemplate       *corev1.Pod
	containerTemplate corev1.Container
	initImage         string
	dindImage         string
	disableDind       bool
	imagePullSecrets  []string
	namespace         string
	timeOut           int
	kuburl            string
	disableServices   bool
	dnsConfig         *corev1.PodDNSConfig
}

// Config is the structure to instantiate a Backend object
type Config struct {
	// Client is the kubernetes clientset
	Client kubernetes.Interface
	// RestConfig is the kubernetes config
	RestConfig *rest.Config
	// Namespace is the namespace in which all actions are performed
	Namespace string
	// ImagePullSecrets is an optional list of image pull secrets that need
	// to be added to the used pod templates
	ImagePullSecrets []string
	// InitImage is the image that is used as init container to prepare vols
	InitImage string
	// DindImage is the image that is used as a sidecar container to
	// support docker-in-docker
	DindImage string
	// DisableDind will disable docker-in-docker support when set to true
	DisableDind bool
	// TimeOut is the max amount of time to wait until a container started
	// or deleted.
	TimeOut time.Duration
	// PodTemplate refers to an optional file containing a pod resource that
	// should be used as the base for creating pod resources.
	PodTemplate string
	// KubedockURL contains the url of this kubedock instance, to be used in
	// docker-in-docker instances/sidecars.
	KubedockURL string

	// Disable the creation of services. A networking solution such as kubedock-dns
	// should be used.
	DisableServices bool
	// DNSServer is the IP address of the DNS server to inject into spawned pods.
	// When set, pods will use DNSPolicy=None and point at this server, with
	// standard Kubernetes search domains preserved. Intended for use with
	// kubedock-dns to provide per-network hostname isolation.
	DNSServer string
}

// New will return a Backend instance.
func New(cfg Config) (Backend, error) {
	pod := &corev1.Pod{}
	if cfg.PodTemplate != "" {
		var err error
		pod, err = podtemplate.PodFromFile(cfg.PodTemplate)
		if err != nil {
			return nil, fmt.Errorf("error opening podtemplate: %w", err)
		}
	}

	var dnsConfig *corev1.PodDNSConfig
	if cfg.DNSServer != "" {
		addr := cfg.DNSServer
		if net.ParseIP(addr) == nil {
			addrs, err := net.LookupHost(addr)
			if err != nil {
				return nil, fmt.Errorf("--dns-server: failed to resolve %q: %w", addr, err)
			}
			addr = addrs[0]
			klog.Infof("Resolved --dns-server %q to %s", cfg.DNSServer, addr)
		}
		ndots := "5"
		dnsConfig = &corev1.PodDNSConfig{
			Nameservers: []string{addr},
			Searches: []string{
				fmt.Sprintf("%s.svc.cluster.local", cfg.Namespace),
				"svc.cluster.local",
				"cluster.local",
			},
			Options: []corev1.PodDNSConfigOption{
				{Name: "ndots", Value: &ndots},
			},
		}
	}

	return &instance{
		cli:               cfg.Client,
		cfg:               cfg.RestConfig,
		initImage:         cfg.InitImage,
		dindImage:         cfg.DindImage,
		disableDind:       cfg.DisableDind,
		namespace:         cfg.Namespace,
		imagePullSecrets:  cfg.ImagePullSecrets,
		podTemplate:       pod,
		containerTemplate: podtemplate.ContainerFromPod(pod),
		kuburl:            cfg.KubedockURL,
		timeOut:           int(cfg.TimeOut.Seconds()),
		disableServices:   cfg.DisableServices,
		dnsConfig:         dnsConfig,
	}, nil
}
