package libpod

import (
	"net/http"

	"github.com/gin-gonic/gin"
	godigest "github.com/opencontainers/go-digest"

	"github.com/joyrex2001/kubedock/internal/events"
	"github.com/joyrex2001/kubedock/internal/model/types"
	"github.com/joyrex2001/kubedock/internal/server/httputil"
	"github.com/joyrex2001/kubedock/internal/server/routes/common"
)

// ImagePull - pull one or more images from a container registry.
// https://docs.podman.io/en/latest/_static/api.html?version=v4.2#tag/images/operation/ImagePullLibpod
// POST "/libpod/images/pull"
func ImagePull(cr *common.ContextRouter, c *gin.Context) {
	from := c.Query("reference")
	// Normalize to canonical form so lookups in ImageJSON and ContainerCreate
	// always find the entry regardless of how the caller spelled the name.
	from = common.NormalizeImageRef(from, cr.Config.RegistryAddr)
	img := &types.Image{Name: from}
	if cr.Config.Inspector {
		digest, cfg, err := cr.Backend.InspectImage(from)
		if err != nil {
			httputil.Error(c, http.StatusInternalServerError, err)
			return
		}
		img.ID = digest.String()
		img.ShortID = digest.Hex()[:12]
		img.Config = cfg
	} else {
		syntheticDigest := godigest.FromBytes([]byte(from))
		img.ID = syntheticDigest.String()
		img.ShortID = syntheticDigest.Hex()[:12]
	}

	if err := cr.DB.SaveImage(img); err != nil {
		httputil.Error(c, http.StatusInternalServerError, err)
		return
	}

	cr.Events.Publish(from, events.Image, events.Pull)

	c.JSON(http.StatusOK, gin.H{
		"Id": img.ID,
	})
}
