package docker

import (
	"net/http"

	"github.com/gin-gonic/gin"
	godigest "github.com/opencontainers/go-digest"

	"github.com/joyrex2001/kubedock/internal/events"
	"github.com/joyrex2001/kubedock/internal/model/types"
	"github.com/joyrex2001/kubedock/internal/server/httputil"
	"github.com/joyrex2001/kubedock/internal/server/routes/common"
)

// ImageCreate - create an image.
// https://docs.docker.com/engine/api/v1.41/#operation/ImageCreate
// POST "/images/create"
func ImageCreate(cr *common.ContextRouter, c *gin.Context) {
	from := c.Query("fromImage")
	tag := c.Query("tag")
	if tag != "" {
		from = from + ":" + tag
	}
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
		"status": "Download complete",
	})
}

// ImagesPrune - Delete unused images.
// https://docs.docker.com/engine/api/v1.41/#operation/ImagePrune
// POST "/images/prune"
func ImagesPrune(cr *common.ContextRouter, c *gin.Context) {
	c.JSON(http.StatusCreated, gin.H{
		"ImagesDeleted":  []string{},
		"SpaceReclaimed": 0,
	})
}
