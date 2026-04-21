package watcher

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/dnsserver/config"
	"github.com/joyrex2001/kubedock/internal/dnsserver/model"
)

// PodAdmin is the interface the watcher uses to notify of pod add/update/delete events.
type PodAdmin interface {
	AddOrUpdate(pod *model.Pod)
	Delete(namespace, name string)
}

// WatchPods starts an informer that watches pods in namespace and forwards
// parsed pod data to pods. It blocks until the stop channel is closed.
func WatchPods(
	clientset kubernetes.Interface,
	namespace string,
	pods PodAdmin,
	podConfig config.PodConfig,
	stop <-chan struct{},
) {
	// Serialize all add/update/delete callbacks through a channel to avoid
	// concurrent mutations to the pod store.
	serializer := make(chan func(), 64)
	go func() {
		for action := range serializer {
			action()
		}
	}()

	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		namespace,
		fields.Everything(),
	)

	addOrUpdate := func(obj interface{}) {
		serializer <- func() {
			k8spod := mustPod(obj)
			pod, err := model.GetPodEssentials(k8spod, "", podConfig)
			if err == nil {
				pods.AddOrUpdate(pod)
			} else {
				klog.V(4).Infof("Ignoring pod %s/%s: %v", k8spod.Namespace, k8spod.Name, err)
			}
		}
	}

	options := cache.InformerOptions{
		ListerWatcher: watchlist,
		ObjectType:    &corev1.Pod{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: addOrUpdate,
			UpdateFunc: func(_ any, obj any) {
				addOrUpdate(obj)
			},
			DeleteFunc: func(obj any) {
				k8spod := mustPod(obj)
				serializer <- func() {
					pods.Delete(k8spod.Namespace, k8spod.Name)
				}
			},
		},
		ResyncPeriod: 0,
	}

	_, controller := cache.NewInformerWithOptions(options)
	go controller.Run(stop)
	<-stop
}

func mustPod(obj any) *corev1.Pod {
	k8spod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.Fatalf("watcher: object of unexpected type: %T", obj)
	}
	return k8spod
}
