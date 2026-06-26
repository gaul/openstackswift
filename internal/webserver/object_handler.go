package webserver

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mdouchement/logger"
	"github.com/mdouchement/openstackswift/internal/database"
	"github.com/mdouchement/openstackswift/internal/model"
	"github.com/mdouchement/openstackswift/internal/storage"
	"github.com/mdouchement/openstackswift/internal/webserver/service"
	"github.com/mdouchement/openstackswift/internal/webserver/weberror"
	"github.com/mdouchement/openstackswift/internal/xpath"
	"github.com/ncw/swift/v2"
)

type object struct {
	logger  logger.Logger
	db      database.Client
	storage storage.Backend
}

func (h *object) setHeadersFromMeta(c echo.Context, metas []*model.Meta) error {
	for _, meta := range metas {
		c.Response().Header().Set(meta.Key, meta.Value)
	}
	return nil
}

// storeObjectMeta persists the request's X-Object-Meta-* headers as object
// metadata.
func (h *object) storeObjectMeta(c echo.Context, containerID, objectKey string) error {
	for key, values := range c.Request().Header {
		if !strings.HasPrefix(key, "X-Object-Meta-") || len(values) == 0 {
			continue
		}
		if _, err := h.db.AddMeta(containerID, objectKey, key, values[0]); err != nil {
			return err
		}
	}
	return nil
}

// setContentHeaders returns the optional content-metadata headers stored with
// the object (Content-Disposition, Content-Encoding), omitting any that are
// unset.
func (h *object) setContentHeaders(c echo.Context, object *model.Object) {
	if object == nil {
		return
	}
	if object.ContentDisposition != "" {
		c.Response().Header().Set("Content-Disposition", object.ContentDisposition)
	}
	if object.ContentEncoding != "" {
		c.Response().Header().Set("Content-Encoding", object.ContentEncoding)
	}
}

func (h *object) Show(c echo.Context) error {
	c.Set("handler_method", "object.Show")

	container, manifest, object, metas, err := h.load(c.Param("container"), c.Param("object"))

	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}
	if manifest == nil && object == nil {
		return weberror.New(http.StatusNotFound, swift.ObjectNotFound.Text)
	}

	//

	if object == nil {
		object = new(model.Object)
		object.CreatedAt = manifest.CreatedAt
		object.ContentType = manifest.ContentType
		object.Size = manifest.Size
		object.Checksum = manifest.Checksum
	}

	//

	h.logger.Debugf("object.Show: meta %v", metas)
	h.setHeadersFromMeta(c, metas)

	c.Response().Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	c.Response().Header().Set("X-Timestamp", strconv.FormatInt(object.CreatedAt.Unix(), 10))
	c.Response().Header().Set("Content-Type", object.ContentType)
	h.setContentHeaders(c, object)
	c.Response().Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	c.Response().Header().Set("Etag", object.Checksum)
	if !object.TTL.IsZero() {
		c.Response().Header().Set("X-Delete-At", strconv.FormatInt(object.TTL.Unix(), 10))
	}
	return nil
}

func (h *object) Download(c echo.Context) error {
	c.Set("handler_method", "object.Download")

	container, manifest, object, metas, err := h.load(c.Param("container"), c.Param("object"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	//

	var downloader service.Downloader
	switch {
	case manifest != nil:
		downloader = service.NewManifestDownloader(h.db, h.storage, container, manifest)
	case object != nil:
		downloader = service.NewObjectDownloader(h.storage, container, object)
	default:
		return weberror.New(http.StatusNotFound, swift.ObjectNotFound.Text)
	}

	//

	r, err := downloader.Stream()
	if err != nil {
		return weberror.New(http.StatusUnprocessableEntity, swift.ObjectCorrupted.Text)
	}
	defer r.Close()

	c.Response().Header().Set("Content-Type", downloader.ContentType())
	c.Response().Header().Set("Etag", downloader.Checksum())
	h.setHeadersFromMeta(c, metas)
	h.setContentHeaders(c, object)
	if object != nil && !object.TTL.IsZero() {
		c.Response().Header().Set("X-Delete-At", strconv.FormatInt(object.TTL.Unix(), 10))
	}

	// Evaluate If-Match / If-None-Match here rather than leaving them to
	// http.ServeContent, whose strong comparison requires RFC-quoted ETags
	// while Swift uses a bare MD5 hex digest.  Delete the headers afterwards
	// so ServeContent does not reject the (unquoted) ETag.
	etag := downloader.Checksum()
	if im := c.Request().Header.Get("If-Match"); im != "" {
		c.Request().Header.Del("If-Match")
		if !etagMatches(im, etag) {
			return c.NoContent(http.StatusPreconditionFailed)
		}
	}
	if inm := c.Request().Header.Get("If-None-Match"); inm != "" {
		c.Request().Header.Del("If-None-Match")
		if etagMatches(inm, etag) {
			return c.NoContent(http.StatusNotModified)
		}
	}

	// http.ServeContent honors Range/If-Range/If-Modified-Since (replying with
	// 206 Partial Content and Content-Range when applicable) as long as the
	// body is seekable.  Single objects are backed by *os.File; multi-segment
	// manifests are not seekable and fall back to a full-body stream.
	if rs, ok := r.(io.ReadSeeker); ok {
		modtime := time.Time{}
		if object != nil && object.CreatedAt != nil {
			modtime = *object.CreatedAt
		}
		http.ServeContent(c.Response(), c.Request(), c.Param("object"), modtime, rs)
		return nil
	}

	c.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(downloader.Size(), 10))
	return c.Stream(http.StatusOK, downloader.ContentType(), r)
}

// etagMatches reports whether a comma-separated If-Match / If-None-Match header
// value matches the given (unquoted) ETag, tolerating surrounding quotes, an
// optional weak prefix, and the "*" wildcard.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.Trim(candidate, `"`)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func (h *object) Update(c echo.Context) error {
	c.Set("handler_method", "object.Update")

	container, manifest, object, metas, err := h.load(c.Param("container"), c.Param("object"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	h.logger.Debug("object.Update: already set meta", metas)

	if manifest == nil && object == nil {
		return weberror.New(http.StatusNotFound, swift.ObjectNotFound.Text)
	}

	if object == nil {
		object = new(model.Object)
		object.CreatedAt = manifest.CreatedAt
		object.ContentType = manifest.ContentType
		object.Size = manifest.Size
		object.Checksum = manifest.Checksum
	}

	//

	if err := h.storeObjectMeta(c, container.ID, c.Param("object")); err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	c.Response().Header().Set("Content-Length", "0")
	c.Response().Header().Set("Content-Type", object.ContentType)
	c.Response().Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	c.Response().Header().Set("X-Timestamp", strconv.FormatInt(object.CreatedAt.Unix(), 10))
	return c.NoContent(http.StatusAccepted)
}

func (h *object) Upload(c echo.Context) error {
	c.Set("handler_method", "object.Upload")

	container, _, object, _, err := h.load(c.Param("container"), c.Param("object"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	//

	if object == nil {
		object = new(model.Object)
	}
	object.ContainerID = container.ID
	object.Key = c.Param("object")
	object.ContentType = c.Request().Header.Get("Content-Type")
	if object.ContentType == "" {
		object.ContentType = echo.MIMEOctetStream
	}
	object.ContentDisposition = c.Request().Header.Get("Content-Disposition")
	object.ContentEncoding = c.Request().Header.Get("Content-Encoding")
	err = service.SetupObjectTTL(object, c.Request())
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	uploader := service.NewObjectUploader(h.storage, container, object)
	err = uploader.Upload(c.Request().Body)
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	if err := h.db.Save(object); err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	if err := h.storeObjectMeta(c, container.ID, object.Key); err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	c.Response().Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	c.Response().Header().Set("X-Timestamp", strconv.FormatInt(object.CreatedAt.Unix(), 10))
	c.Response().Header().Set("Content-Type", object.ContentType)
	c.Response().Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	c.Response().Header().Set("Etag", object.Checksum)
	return c.NoContent(http.StatusCreated)
}

func (h *object) Manifest(c echo.Context) error {
	c.Set("handler_method", "object.Manifest")

	container, manifest, _, _, err := h.load(c.Param("container"), c.Param("object"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	//

	if manifest == nil {
		manifest = new(model.Manifest)
	}
	manifest.ContainerID = container.ID
	manifest.Key = c.Param("object")
	manifest.ContentType = c.Request().Header.Get("Content-Type")

	mc := service.NewManifestCreation(h.db, h.storage, container, manifest)
	err = mc.Create(c.Request().Header.Get("X-Object-Manifest"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	if err := h.db.Save(manifest); err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	c.Response().Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	c.Response().Header().Set("X-Timestamp", strconv.FormatInt(manifest.CreatedAt.Unix(), 10))
	return c.NoContent(http.StatusCreated)
}

func (h *object) Copy(c echo.Context) error {
	c.Set("handler_method", "object.Copy")

	path := c.Get("object_source").(string)
	cname, oname := xpath.Entities(path)

	//

	container, manifest, object, _, err := h.load(cname, oname)
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	//

	var copier service.Copier
	switch {
	case manifest != nil:
		copier = service.NewManifestCopier(h.db, h.storage, container, manifest)
	case object != nil:
		copier = service.NewObjectCopier(h.db, h.storage, container, object)
	default:
		return weberror.New(http.StatusNotFound, swift.ObjectNotFound.Text)
	}

	//

	path = c.Get("object_destination").(string)
	cname, oname = xpath.Entities(path)

	err = copier.Copy(cname, oname)
	if err == swift.TooLargeObject || err == swift.ObjectCorrupted {
		return weberror.New(err.(*swift.Error).StatusCode, err.Error())
	}
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	c.Response().Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	c.Response().Header().Set("X-Timestamp", strconv.FormatInt(copier.CreatedAt().Unix(), 10))
	c.Response().Header().Set("Etag", copier.Checksum())
	return nil
}

func (h *object) Delete(c echo.Context) error {
	c.Set("handler_method", "object.Delete")

	container, manifest, object, _, err := h.load(c.Param("container"), c.Param("object"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	//

	var destroyer service.Destroyer
	switch {
	case manifest != nil:
		destroyer = service.NewManifestDestroyer(h.db, h.storage, container, manifest)
	case object != nil:
		destroyer = service.NewObjectDestroyer(h.db, h.storage, container, object)
	default:
		return weberror.New(http.StatusNotFound, swift.ObjectNotFound.Text)
	}

	//

	err = destroyer.Destroy()
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}

	//

	return c.NoContent(http.StatusNoContent)
}

func (h *object) load(containername, objectname string) (*model.Container, *model.Manifest, *model.Object, []*model.Meta, error) {
	container, err := h.db.FindContainerByName(containername)
	if err != nil {
		if h.db.IsNotFound(err) {
			return nil, nil, nil, nil, nil
		}
		return nil, nil, nil, nil, err
	}

	//

	object, err := h.db.FindObjectByKey(container.ID, objectname)
	if err != nil && !h.db.IsNotFound(err) {
		return container, nil, nil, nil, err
	}
	if h.db.IsNotFound(err) {
		object = nil
	}

	//

	var manifest *model.Manifest = nil

	if object == nil {
		// Only fetch if no object is found
		manifest, err = h.db.FindManifestByKey(container.ID, objectname)
		if err != nil && !h.db.IsNotFound(err) {
			return container, nil, nil, nil, err
		}
		if h.db.IsNotFound(err) {
			manifest = nil
		}
	}

	//

	var metas []*model.Meta = nil

	// Fetch meta data for manifest or object
	if object != nil || manifest != nil {
		metas, err = h.db.FindMeta(container.ID, objectname)

		if err != nil && !h.db.IsNotFound(err) {
			return container, manifest, object, nil, err
		}
	}

	// always empty list
	if metas == nil {
		metas = make([]*model.Meta, 0)
	}

	return container, manifest, object, metas, nil
}
