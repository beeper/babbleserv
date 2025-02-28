package federation

import (
	"net/http"

	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/server-server-api/#get_matrixfederationv1mediadownloadmediaid
func (f *FederationRoutes) DownloadMedia(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.11/server-server-api/#get_matrixfederationv1mediathumbnailmediaid
func (f *FederationRoutes) DownloadThumbnail(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
