package config

// Annotation and label constants used by kubedock when spawning pods and by
// the dns-server when watching pods for hostname resolution.
const (
	// HostAliasAnnotationPrefix is the annotation prefix used to specify
	// hostname aliases for a container (e.g. "kubedock.hostalias/0" = "db").
	HostAliasAnnotationPrefix = "kubedock.hostalias/"

	// NetworkAnnotationPrefix is the annotation prefix used to specify
	// which networks a container belongs to (e.g. "kubedock.network/0" = "mynet").
	NetworkAnnotationPrefix = "kubedock.network/"

	// KubedockPodLabel is the label key (with value "true") set on every pod
	// spawned by kubedock. The dns-server uses this to filter which pods to watch.
	KubedockPodLabel = "kubedock"
)
