package storage

import (
	"io"
	fspkg "io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
)

type fs struct {
	workspace string
}

// NewFileSystem returns a new File System backend.
func NewFileSystem(workspace string) Backend {
	return &fs{
		workspace: workspace,
	}
}

func (b *fs) Name() string {
	return "file_system"
}

func (b *fs) Reader(container, object string) (io.ReadCloser, error) {
	p, err := b.objectPath(container, object)
	if err != nil {
		return nil, err
	}
	rc, err := os.Open(p)
	if err != nil {
		return rc, errors.Wrap(err, "could not open file")
	}
	return rc, err
}

func (b *fs) Writer(container, object string) (io.WriteCloser, error) {
	p, err := b.objectPath(container, object)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return nil, errors.Wrap(err, "could not create directory")
	}

	wc, err := os.Create(p)
	if err != nil {
		return wc, errors.Wrap(err, "could not create file")
	}
	return wc, err
}

func (b *fs) Copy(sc, so, dc, do string) error {
	srcPath, err := b.objectPath(sc, so)
	if err != nil {
		return errors.Wrap(err, "copy: source")
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return errors.Wrap(err, "copy: source")
	}
	defer src.Close()

	//

	dstPath, err := b.objectPath(dc, do)
	if err != nil {
		return errors.Wrap(err, "copy: destination")
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return errors.Wrap(err, "copy: destination")
	}

	dst, err := os.Create(dstPath)
	if err != nil {
		return errors.Wrap(err, "copy: destination")
	}
	defer dst.Close()

	//

	_, err = io.Copy(dst, src)
	if err != nil {
		return errors.Wrap(err, "copy")
	}

	err = dst.Sync()
	return errors.Wrap(err, "copy: destination")
}

func (b *fs) FilenamesFrom(prefix string) ([]string, error) {
	p, err := secureJoin(b.workspace, prefix)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}

	var filenames []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filenames = append(filenames, entry.Name())
	}

	return filenames, nil
}

func (b *fs) Remove(container, object string) error {
	p, err := b.objectPath(container, object)
	if err != nil {
		return errors.Wrap(err, "could not delete file")
	}
	if err := os.RemoveAll(p); err != nil {
		return errors.Wrap(err, "could not delete file")
	}
	return nil
}

func (b *fs) RemoveAll(path string) error {
	return b.Remove(path, "")
}

func (b *fs) Cleanup() error {
	// Find empty directories.
	//
	stats := map[string]int{}
	err := filepath.Walk(b.workspace, func(path string, info fspkg.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			if path == b.workspace {
				return nil
			}
			stats[path] = 0
			return nil
		}

		if strings.HasSuffix(path, ".DS_Store") {
			return nil
		}

		trimmedpath := strings.Replace(path, b.workspace, "", 1)
		base := b.workspace

		for _, segment := range strings.Split(filepath.Dir(trimmedpath), string(os.PathSeparator)) {
			base = filepath.Join(base, segment)
			if !strings.HasPrefix(base, b.workspace) {
				continue
			}
			stats[base]++
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "cleanup")
	}

	// Remove empty directories.
	//
	for dirname, count := range stats {
		if count == 0 {
			os.RemoveAll(dirname)
		}
	}
	return nil
}

// objectPath resolves the on-disk path for an object, ensuring neither the
// container nor the object name escapes the container directory via "../"
// traversal.  Object names legitimately contain "/" (pseudo-directories), so
// only escapes beyond the container root are rejected.
func (b *fs) objectPath(container, object string) (string, error) {
	root, err := secureJoin(b.workspace, container)
	if err != nil {
		return "", err
	}
	return secureJoin(root, encodeObjectName(object))
}

// encodeObjectName makes an object key safe to use as an on-disk path.
//
// Interior "/" are deliberately left intact and keep mirroring the directory
// layout, so two keys where one is a directory prefix of the other ("foo/bar"
// and "foo/bar/baz") still cannot coexist -- the same filesystem limitation as
// the S3Proxy nio2 backend.  A *trailing* "/" is different: it would leave an
// empty final path component that os.Create cannot create, so encode any
// trailing slash run.  "%" is escaped first to keep the mapping unambiguous
// (e.g. so a literal "asdf%2F" key never collides with "asdf/").  Listings are
// served from the metadata database, not these names, so the encoding stays
// internal to the storage backend.
func encodeObjectName(object string) string {
	if !strings.HasSuffix(object, "/") && !strings.Contains(object, "%") {
		return object
	}
	object = strings.ReplaceAll(object, "%", "%25")
	n := len(object)
	for n > 0 && object[n-1] == '/' {
		n--
	}
	return object[:n] + strings.Repeat("%2F", len(object)-n)
}

// secureJoin joins name onto root and verifies the cleaned result stays within
// root, guarding against "../" path traversal in untrusted container and object
// names.
func secureJoin(root, name string) (string, error) {
	p := filepath.Join(root, name)
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", errors.Errorf("invalid path: %q escapes %q", name, root)
	}
	return p, nil
}
