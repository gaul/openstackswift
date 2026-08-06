package database

import (
	"sort"
	"strings"
	"time"

	"github.com/asdine/storm/v3"
	"github.com/asdine/storm/v3/codec/json"
	"github.com/asdine/storm/v3/q"
	"github.com/gofrs/uuid"
	"github.com/mdouchement/openstackswift/internal/model"
	"github.com/pkg/errors"
)

type strm struct {
	db *storm.DB
}

// StormCodec is the format used to store data in the database.
var StormCodec = storm.Codec(json.Codec)

// StormInit initializes Storm database.
func StormInit(database string) error {
	db, err := storm.Open(database, StormCodec)
	if err != nil {
		return errors.Wrap(err, "could not get database connection")
	}

	if err := db.Init(&model.Container{}); err != nil {
		return errors.Wrap(err, "could not init container index")
	}

	if err := db.Init(&model.Manifest{}); err != nil {
		return errors.Wrap(err, "could not init manifest index")
	}

	err = db.Init(&model.Object{})
	return errors.Wrap(err, "could not init object index")
}

func StormReIndex(database string) error {
	db, err := storm.Open(database, StormCodec)
	if err != nil {
		return errors.Wrap(err, "could not get database connection")
	}

	if err := db.ReIndex(&model.Container{}); err != nil {
		return errors.Wrap(err, "could not ReIndex containers")
	}

	if err := db.ReIndex(&model.Manifest{}); err != nil {
		return errors.Wrap(err, "could not ReIndex manifests")
	}

	err = db.ReIndex(&model.Object{})
	return errors.Wrap(err, "could not ReIndex objects")
}

func StormOpen(database string) (Client, error) {
	db, err := storm.Open(database, StormCodec)
	if err != nil {
		return nil, errors.Wrap(err, "could not get database connection")
	}

	return &strm{
		db: db,
	}, nil
}

func (c *strm) Save(m model.Model) error {
	t := time.Now().UTC()
	m.SetUpdatedAt(t)

	if m.GetID() == "" {
		m.SetID(uuid.Must(uuid.NewV4()).String())
		m.SetCreatedAt(t)
	}

	if k, ok := m.(model.Keyed); ok {
		k.SyncKeys()
	}

	return errors.Wrap(c.db.Save(m), "could not save the model")
}

func (c *strm) Delete(m model.Model) error {
	return errors.Wrap(c.db.DeleteStruct(m), "could not delete the model")
}

func (c *strm) Close() error {
	return c.db.Close()
}

func (c *strm) IsNotFound(err error) bool {
	return errors.Cause(err) == storm.ErrNotFound
}

//
// Container
//

func (c *strm) ListContainers() ([]*model.Container, error) {
	containers := make([]*model.Container, 0)
	err := c.db.AllByIndex("Name", &containers)
	if c.IsNotFound(err) {
		// An empty account lists no containers; not an error.
		return containers, nil
	}
	return containers, errors.Wrap(err, "could not get all containers")
}

func (c *strm) FindContainer(id string) (*model.Container, error) {
	var container model.Container
	err := c.db.One("ID", id, &container)
	return &container, errors.Wrap(err, "could not find container")
}

func (c *strm) FindContainerByName(name string) (*model.Container, error) {
	var container model.Container
	err := c.db.One("Name", name, &container)
	return &container, errors.Wrap(err, "could not find container")
}

func (c *strm) DeleteContainer(id string) error {
	var container model.Container
	if err := c.db.One("ID", id, &container); err != nil {
		return errors.Wrap(err, "could not delete container")
	}
	return errors.Wrap(c.db.DeleteStruct(&container),
		"could not delete container")
}

//
// Object
//

func (c *strm) AllObjects() ([]*model.Object, error) {
	objects := make([]*model.Object, 0)
	err := c.db.All(&objects)
	if c.IsNotFound(err) {
		// An empty database lists no objects; not an error.
		return objects, nil
	}
	return objects, errors.Wrap(err, "could not get all objects")
}

func (c *strm) FindObjectsByContainerID(id string, limit int, prefix, marker string) ([]*model.Object, error) {
	// Read the container's objects through the ContainerID index rather
	// than scanning every object in the database, then apply the listing
	// rules to that slice: prefix is a literal object-name prefix (Swift
	// listing semantics), and marker returns only the keys sorting strictly
	// after it, so the limit applies to the page that follows.
	all := make([]*model.Object, 0)
	err := c.db.Find("ContainerID", id, &all)
	if err != nil {
		if c.IsNotFound(err) {
			return all, nil
		}
		return all, errors.Wrap(err, "could not get objects by container_id")
	}

	objects := make([]*model.Object, 0, len(all))
	for _, object := range all {
		if object.ContainerID != id {
			continue
		}
		if !strings.HasPrefix(object.Key, prefix) {
			continue
		}
		if marker != "" && object.Key <= marker {
			continue
		}
		objects = append(objects, object)
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})
	if limit > 0 && len(objects) > limit {
		objects = objects[:limit]
	}
	return objects, nil
}

func (c *strm) FindObjectsByManifestID(id string) ([]*model.Object, error) {
	objects := make([]*model.Object, 0)
	err := c.db.Select(q.Eq("ManifestID", id)).OrderBy("CreatedAt").OrderBy("Key").Find(&objects)
	return objects, errors.Wrap(err, "could not get objects by manifest_id")
}

// Storm stores a list-index entry as value + "__" + id and looks one up by
// that prefix, so a value another value extends -- "a" beside "a__b" -- finds
// the wrong record.  Read the index for candidates, which narrows thousands
// of objects to a handful, then confirm the fields it stands for.
func (c *strm) FindObjectByKey(cid, key string) (*model.Object, error) {
	candidates := make([]*model.Object, 0)
	if err := c.db.Find("CKey", model.ScopedKey(cid, key), &candidates); err != nil {
		return nil, errors.Wrap(err, "could not find object")
	}
	for _, object := range candidates {
		if object.ContainerID == cid && object.Key == key {
			return object, nil
		}
	}
	return nil, errors.Wrap(storm.ErrNotFound, "could not find object")
}

func (c *strm) DeleteObject(id string) error {
	var object model.Object
	if err := c.db.One("ID", id, &object); err != nil {
		return errors.Wrap(err, "could not delete object")
	}
	return errors.Wrap(c.db.DeleteStruct(&object), "could not delete object")
}

//
// Manifest
//

func (c *strm) FindManifestByKey(cid, key string) (*model.Manifest, error) {
	candidates := make([]*model.Manifest, 0)
	if err := c.db.Find("CKey", model.ScopedKey(cid, key), &candidates); err != nil {
		return nil, errors.Wrap(err, "could not find manifest")
	}
	for _, manifest := range candidates {
		if manifest.ContainerID == cid && manifest.Key == key {
			return manifest, nil
		}
	}
	return nil, errors.Wrap(storm.ErrNotFound, "could not find manifest")
}

func (c *strm) DeleteManifest(id string) error {
	var manifest model.Manifest
	if err := c.db.One("ID", id, &manifest); err != nil {
		return errors.Wrap(err, "could not delete manifest")
	}
	return errors.Wrap(c.db.DeleteStruct(&manifest),
		"could not delete manifest")
}

//
// Meta
//
func (c *strm) AddMeta(cid, okey string, key string, value string) (*model.Meta, error) {
	var meta_model = new(model.Meta)
	meta_model.ContainerID = cid
	meta_model.ObjectKey = okey
	meta_model.Key = key
	meta_model.Value = value
	if err := c.Save(meta_model); err != nil {
		return nil, errors.Wrap(err, "could not save meta")
	}
	return meta_model, nil
}

func (c *strm) FindMeta(cid, okey string) ([]*model.Meta, error) {
	candidates := make([]*model.Meta, 0)
	if err := c.db.Find("OKey", model.ScopedKey(cid, okey), &candidates); err != nil {
		return nil, errors.Wrap(err, "could not find metas")
	}
	// see FindObjectByKey on why the index answers with candidates
	metas := make([]*model.Meta, 0, len(candidates))
	for _, meta := range candidates {
		if meta.ContainerID == cid && meta.ObjectKey == okey {
			metas = append(metas, meta)
		}
	}
	if len(metas) == 0 {
		return metas, errors.Wrap(storm.ErrNotFound, "could not find metas")
	}
	return metas, nil
}

func (c *strm) DeleteMeta(cid, okey string, key string) error {
	metas, err := c.FindMeta(cid, okey)
	if err != nil {
		return errors.Wrap(err, "could not delete meta")
	}
	for _, meta := range metas {
		if meta.Key != key {
			continue
		}
		if err := c.db.DeleteStruct(meta); err != nil {
			return errors.Wrap(err, "could not delete meta")
		}
	}
	return nil
}

func (c *strm) DeleteAllMetas(cid, okey string) error {
	metas, err := c.FindMeta(cid, okey)
	if err != nil {
		return errors.Wrap(err, "could not delete all metas")
	}
	for _, meta := range metas {
		if err := c.db.DeleteStruct(meta); err != nil {
			return errors.Wrap(err, "could not delete all metas")
		}
	}
	return nil
}
