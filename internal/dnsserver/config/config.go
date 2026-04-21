package config

// PodConfig holds the annotation/label names that kubedock-dns uses to read
// per-pod network configuration.
type PodConfig struct {
	HostAliasPrefix string
	NetworkIdPrefix string
	LabelName       string
}
