package common

import (
	"fmt"
	"time"

	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/backend"
	"github.com/joyrex2001/kubedock/internal/model/types"
)

// StartContainer will start given container and saves the appropriate state
// in the database.
func StartContainer(cr *ContextRouter, tainr *types.Container) error {
	resolveRegistryRef(cr, tainr)
	state, err := cr.Backend.StartContainer(tainr)
	if err != nil {
		return err
	}

	tainr.HostIP = "0.0.0.0"
	if cr.Config.PortForward {
		cr.Backend.CreatePortForwards(tainr)
	} else {
		if len(tainr.GetServicePorts()) > 0 {
			ip, err := cr.Backend.GetPodIP(tainr)
			if err != nil {
				return err
			}
			tainr.HostIP = ip
			if cr.Config.ReverseProxy {
				cr.Backend.CreateReverseProxies(tainr)
			}
		}
	}

	tainr.Stopped = false
	tainr.Killed = false
	tainr.Failed = (state == backend.DeployFailed)
	tainr.Completed = (state == backend.DeployCompleted)
	tainr.Running = (state == backend.DeployRunning)

	return cr.DB.SaveContainer(tainr)
}

// StartInspectionContainer starts a container with a busybox sleep entrypoint
// injected via an init container, so the pod stays alive for docker cp / exec
// regardless of what the image contains. Used by GetArchive/HeadArchive for
// tools like container_structure_test.
//
// If the container was previously started and failed (pod cleaned up), the
// failure state is reset so a fresh pod can be created.
func StartInspectionContainer(cr *ContextRouter, tainr *types.Container) error {
	if tainr.Running {
		// Verify the pod is still alive — it may have been reaped while the DB
		// still shows Running=true. If the pod is gone or not running, fall
		// through to restart it rather than returning immediately (which would
		// cause a subsequent exec to fail with "cannot exec in a deleted state").
		status, err := cr.Backend.GetContainerStatus(tainr)
		if err == nil && status == backend.DeployRunning {
			return nil
		}
		klog.V(2).Infof("inspection pod %s is no longer running (status=%v err=%v); restarting", tainr.GetPodName(), status, err)
		_ = cr.Backend.DeleteContainer(tainr)
		tainr.Running = false
		tainr.Failed = false
		tainr.Completed = false
		tainr.Stopped = false
	}
	// Reset any prior non-running state so a fresh pod can be created.
	if tainr.Failed || tainr.Completed || tainr.Stopped {
		_ = cr.Backend.DeleteContainer(tainr) // no-op if pod is already gone
		tainr.Failed = false
		tainr.Completed = false
		tainr.Stopped = false
	}

	resolveRegistryRef(cr, tainr)
	state, err := cr.Backend.StartContainerForInspection(tainr)
	if err != nil {
		return err
	}
	if state != backend.DeployRunning {
		return fmt.Errorf("inspection container failed to start (state=%v)", state)
	}

	tainr.HostIP = "0.0.0.0"
	tainr.Stopped = false
	tainr.Killed = false
	tainr.Failed = false
	tainr.Completed = false
	tainr.Running = true

	return cr.DB.SaveContainer(tainr)
}

// resolveRegistryRef resolves tainr.Image to a K8s-pullable reference.
//
// RegistryRef takes priority: it is the digest-pinned CAS URL set by ImageLoad
// for images that were docker-loaded (arbitrary tags, not necessarily pullable).
// For images registered via docker pull, RegistryRef is empty and img.Name
// (which NormalizeImageRef already expanded with the registry hostname) is used.
func resolveRegistryRef(cr *ContextRouter, tainr *types.Container) {
	img, err := cr.DB.GetImageByNameOrID(NormalizeImageRef(tainr.Image, cr.Config.RegistryAddr))
	if err != nil {
		return
	}
	if img.RegistryRef != "" && img.RegistryRef != tainr.Image {
		klog.Infof("resolving image %s to registry ref %s", tainr.Image, img.RegistryRef)
		tainr.Image = img.RegistryRef
	} else if img.Name != "" && img.Name != tainr.Image {
		klog.V(3).Infof("resolving image %s to normalized name %s", tainr.Image, img.Name)
		tainr.Image = img.Name
	}
}

// UpdateContainerStatus will check if the started container is finished and will
// update the container database record accordingly.
func UpdateContainerStatus(cr *ContextRouter, tainr *types.Container) {
	if tainr.Completed {
		return
	}
	if !cr.Limiter.Allow() {
		klog.V(2).Infof("rate-limited status request for container: %s", tainr.ID)
		return
	}
	status, err := cr.Backend.GetContainerStatus(tainr)
	if err != nil {
		klog.Warningf("container status error: %s", err)
		tainr.Failed = true
	}
	if status == backend.DeployCompleted {
		tainr.Finished = time.Now()
		tainr.Completed = true
		tainr.Running = false
	}
}
