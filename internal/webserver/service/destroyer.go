package service

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mdouchement/openstackswift/internal/database"
	"github.com/mdouchement/openstackswift/internal/model"
	"github.com/mdouchement/openstackswift/internal/storage"
	"github.com/pkg/errors"
)

// A Destroyer removes file(s) from storage.
type Destroyer interface {
	Destroy() error
}

// SetupObjectTTL configures the time to live to live according the requests headers.
func SetupObjectTTL(m *model.Object, r *http.Request) error {
	m.TTL = time.Time{} // Reset

	delete := r.Header.Get("X-Delete-After")
	if delete != "" {
		seconds, err := strconv.ParseInt(delete, 10, 64)
		if err != nil {
			return errors.Wrap(err, "X-Delete-After")
		}

		m.TTL = time.Now().Add(time.Duration(seconds) * time.Second)
		return nil
	}

	delete = r.Header.Get("X-Delete-At")
	if delete != "" {
		unix, err := strconv.ParseInt(delete, 10, 64)
		if err != nil {
			return errors.Wrap(err, "X-Delete-At")
		}

		m.TTL = time.Unix(unix, 0)
	}

	return nil
}

//
//-----
//

// An ObjectDestroyer removes the file from storage.
type ObjectDestroyer struct {
	database  database.Client
	storage   storage.Backend
	container *model.Container
	object    *model.Object
}

// NewObjectDestroyer returns a new ObjectDestroyer.
func NewObjectDestroyer(database database.Client, storage storage.Backend, container *model.Container, object *model.Object) Destroyer {
	return &ObjectDestroyer{
		database:  database,
		storage:   storage,
		container: container,
		object:    object,
	}
}

func (s *ObjectDestroyer) Destroy() error {
	err := s.storage.Remove(s.container.Name, s.object.Key)
	if err != nil {
		return errors.Wrap(err, "ObjectDestroyer storage")
	}

	err = s.database.DeleteAllMetas(s.container.ID, s.object.ID)
	if err != nil && !s.database.IsNotFound(err) {
		return errors.Wrap(err, "ObjectDestroyer meta")
	}

	err = s.database.DeleteObject(s.object.ID)
	return errors.Wrap(err, "ObjectDestroyer object")
}

//
//-----
//

// An ManifestDestroyer removes the segment objects from storage.
type ManifestDestroyer struct {
	database  database.Client
	storage   storage.Backend
	container *model.Container
	manifest  *model.Manifest
}

// NewManifestDestroyer returns a new ManifestDestroyer.
func NewManifestDestroyer(database database.Client, storage storage.Backend, container *model.Container, manifest *model.Manifest) Destroyer {
	return &ManifestDestroyer{
		database:  database,
		storage:   storage,
		container: container,
		manifest:  manifest,
	}
}

func (s *ManifestDestroyer) Destroy() error {
	objects, err := s.database.FindObjectsByManifestID(s.manifest.ID)
	if err != nil {
		return errors.Wrap(err, "ManifestDestroyer find manifest")
	}

	for _, object := range objects {
		container, err := s.database.FindContainer(object.ContainerID)
		if err != nil {
			return errors.Wrap(err, "ManifestDestroyer find container")
		}

		err = s.storage.Remove(container.Name, object.Key)
		if err != nil {
			return errors.Wrap(err, "ManifestDestroyer storage")
		}

		err = s.database.DeleteObject(object.ID)
		if err != nil {
			return errors.Wrap(err, "ManifestDestroyer object")
		}
	}

	err = s.database.DeleteAllMetas(s.container.ID, s.manifest.ID)
	if err != nil && !s.database.IsNotFound(err) {
		return errors.Wrap(err, "ManifestDestroyer meta")
    }

	//

	err = s.database.DeleteManifest(s.manifest.ID)
	return errors.Wrap(err, "ManifestDestroyer manifest")
}

//
//-----
//

// A StaticObjectDestroyer removes a Static Large Object (SLO) manifest.  When
// withSegments is set (DELETE ...?multipart-manifest=delete) it also removes the
// referenced segment objects; otherwise the segments are left in place, matching
// Swift, where a plain DELETE removes only the manifest.
type StaticObjectDestroyer struct {
	database     database.Client
	storage      storage.Backend
	container    *model.Container
	object       *model.Object
	withSegments bool
}

// NewStaticObjectDestroyer returns a new StaticObjectDestroyer.
func NewStaticObjectDestroyer(database database.Client, storage storage.Backend, container *model.Container, object *model.Object, withSegments bool) Destroyer {
	return &StaticObjectDestroyer{
		database:     database,
		storage:      storage,
		container:    container,
		object:       object,
		withSegments: withSegments,
	}
}

func (s *StaticObjectDestroyer) Destroy() error {
	if s.withSegments {
		for _, segment := range s.object.Segments {
			container, err := s.database.FindContainerByName(segment.Container)
			if err != nil {
				if s.database.IsNotFound(err) {
					continue
				}
				return errors.Wrap(err, "StaticObjectDestroyer find container")
			}

			if err = s.storage.Remove(segment.Container, segment.Object); err != nil {
				return errors.Wrap(err, "StaticObjectDestroyer storage")
			}

			object, err := s.database.FindObjectByKey(container.ID, segment.Object)
			if err != nil {
				if s.database.IsNotFound(err) {
					continue
				}
				return errors.Wrap(err, "StaticObjectDestroyer find object")
			}

			err = s.database.DeleteAllMetas(container.ID, segment.Object)
			if err != nil && !s.database.IsNotFound(err) {
				return errors.Wrap(err, "StaticObjectDestroyer meta")
			}

			if err = s.database.DeleteObject(object.ID); err != nil {
				return errors.Wrap(err, "StaticObjectDestroyer object")
			}
		}
	}

	// The manifest object has no backing file, so only its records are removed.
	err := s.database.DeleteAllMetas(s.container.ID, s.object.Key)
	if err != nil && !s.database.IsNotFound(err) {
		return errors.Wrap(err, "StaticObjectDestroyer meta")
	}

	return errors.Wrap(s.database.DeleteObject(s.object.ID), "StaticObjectDestroyer object")
}
