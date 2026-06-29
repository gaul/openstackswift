package webserver

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
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

// storeObjectMeta replaces the object's user metadata with the request's
// X-Object-Meta-* headers.  A PUT or POST sets the metadata wholesale, so any
// metadata persisted by an earlier write is dropped first.
func (h *object) storeObjectMeta(c echo.Context, containerID, objectKey string) error {
	if err := h.db.DeleteAllMetas(containerID, objectKey); err != nil &&
		!h.db.IsNotFound(err) {
		return err
	}
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
	if object.Static {
		c.Response().Header().Set("X-Static-Large-Object", "true")
	}
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

	// GET ...?multipart-manifest=get returns the SLO manifest itself (the
	// ordered segment list) rather than the concatenated large object.
	if c.QueryParam("multipart-manifest") == "get" && object != nil && object.Static {
		segments := make([]map[string]interface{}, 0, len(object.Segments))
		for _, segment := range object.Segments {
			segments = append(segments, map[string]interface{}{
				"name":  "/" + segment.Container + "/" + segment.Object,
				"hash":  segment.Etag,
				"bytes": segment.Size,
			})
		}
		return c.JSON(http.StatusOK, segments)
	}

	//

	var downloader service.Downloader
	switch {
	case object != nil && object.Static:
		downloader = service.NewStaticObjectDownloader(h.storage, object)
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
	if object != nil && object.Static {
		c.Response().Header().Set("X-Static-Large-Object", "true")
	}
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
	// A normal PUT over an existing SLO manifest turns it back into a regular,
	// file-backed object.
	object.Static = false
	object.Segments = nil
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

// StaticManifest handles PUT ...?multipart-manifest=put, creating a Static Large
// Object.  The request body is a JSON array of {path, etag, size_bytes} segment
// descriptors; the resulting object's content is the concatenation of those
// segments and its ETag is the MD5 of their concatenated ETags.
func (h *object) StaticManifest(c echo.Context) error {
	c.Set("handler_method", "object.StaticManifest")

	container, _, existing, _, err := h.load(c.Param("container"), c.Param("object"))
	if err != nil {
		return weberror.New(http.StatusInternalServerError, err.Error())
	}
	if container == nil {
		return weberror.New(http.StatusNotFound, swift.ContainerNotFound.Text)
	}

	//

	var entries []struct {
		Path      string `json:"path"`
		Etag      string `json:"etag"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&entries); err != nil {
		return weberror.New(http.StatusBadRequest, "invalid SLO manifest: "+err.Error())
	}
	if len(entries) == 0 {
		return weberror.New(http.StatusBadRequest, "SLO manifest must list at least one segment")
	}

	//

	segments := make([]model.Segment, 0, len(entries))
	var size int64
	h5 := md5.New()
	for _, entry := range entries {
		scontainerName, skey := xpath.Entities(entry.Path)
		scontainer, err := h.db.FindContainerByName(scontainerName)
		if err != nil {
			if h.db.IsNotFound(err) {
				return weberror.New(http.StatusBadRequest, "SLO segment not found: "+entry.Path)
			}
			return weberror.New(http.StatusInternalServerError, err.Error())
		}
		sobject, err := h.db.FindObjectByKey(scontainer.ID, skey)
		if err != nil {
			if h.db.IsNotFound(err) {
				return weberror.New(http.StatusBadRequest, "SLO segment not found: "+entry.Path)
			}
			return weberror.New(http.StatusInternalServerError, err.Error())
		}
		if entry.Etag != "" && !strings.EqualFold(entry.Etag, sobject.Checksum) {
			return weberror.New(http.StatusBadRequest, "SLO segment etag mismatch: "+entry.Path)
		}
		if entry.SizeBytes != 0 && entry.SizeBytes != sobject.Size {
			return weberror.New(http.StatusBadRequest, "SLO segment size mismatch: "+entry.Path)
		}

		segments = append(segments, model.Segment{
			Container: scontainerName,
			Object:    skey,
			Size:      sobject.Size,
			Etag:      sobject.Checksum,
		})
		size += sobject.Size
		h5.Write([]byte(sobject.Checksum))
	}

	//

	object := existing
	if object == nil {
		object = new(model.Object)
	} else if !object.Static {
		// Replacing a regular, file-backed object with a manifest; drop its file.
		_ = h.storage.Remove(container.Name, object.Key)
	}
	object.ContainerID = container.ID
	object.ManifestID = ""
	object.Key = c.Param("object")
	object.ContentType = c.Request().Header.Get("Content-Type")
	if object.ContentType == "" {
		object.ContentType = echo.MIMEOctetStream
	}
	object.ContentDisposition = c.Request().Header.Get("Content-Disposition")
	object.ContentEncoding = c.Request().Header.Get("Content-Encoding")
	object.Size = size
	object.Checksum = hex.EncodeToString(h5.Sum(nil))
	object.Static = true
	object.Segments = segments
	object.TTL = time.Time{}

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
	c.Response().Header().Set("Content-Length", "0")
	c.Response().Header().Set("Etag", object.Checksum)
	c.Response().Header().Set("X-Static-Large-Object", "true")
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
	case object != nil && object.Static:
		copier = service.NewStaticObjectCopier(h.db, h.storage, container, object)
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
	case object != nil && object.Static:
		// DELETE ...?multipart-manifest=delete removes the segments too;
		// otherwise only the manifest object is removed.
		withSegments := c.QueryParam("multipart-manifest") == "delete"
		destroyer = service.NewStaticObjectDestroyer(h.db, h.storage, container, object, withSegments)
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
