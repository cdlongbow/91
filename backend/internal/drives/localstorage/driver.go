// Package localstorage exposes an existing server-side directory as a Drive.
package localstorage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/video-site/backend/internal/drives"
)

const Kind = "localstorage"

type Config struct {
	ID       string
	RootPath string
}

type Driver struct {
	id       string
	rootPath string
}

func New(c Config) *Driver {
	return &Driver{
		id:       c.ID,
		rootPath: c.RootPath,
	}
}

func (d *Driver) Kind() string { return Kind }

func (d *Driver) ID() string { return d.id }

func (d *Driver) RootID() string { return "/" }

func (d *Driver) Init(context.Context) error {
	root, err := d.root()
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("localstorage: stat root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("localstorage: root is not a directory: %s", root)
	}
	return nil
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	dir, rel, err := d.pathForID(dirID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]drives.Entry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Symlinks can escape the configured root or create cycles. Keep the
		// local storage drive predictable by scanning real files/directories only.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		childRel := joinRel(rel, entry.Name())
		out = append(out, drives.Entry{
			ID:       encodeRel(childRel),
			Name:     entry.Name(),
			Size:     sizeForEntry(info),
			IsDir:    info.IsDir(),
			ParentID: idForRel(rel),
			ModTime:  info.ModTime(),
		})
	}
	return out, nil
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	p, rel, err := d.pathForID(fileID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return &drives.Entry{
		ID:       idForRel(rel),
		Name:     filepath.Base(p),
		Size:     sizeForEntry(info),
		IsDir:    info.IsDir(),
		ParentID: idForRel(parentRel(rel)),
		ModTime:  info.ModTime(),
	}, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	p, _, err := d.pathForID(fileID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, os.ErrNotExist
	}
	return &drives.StreamLink{
		URL:     p,
		Expires: time.Now().Add(24 * time.Hour),
	}, nil
}

func (d *Driver) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}

func (d *Driver) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}

func (d *Driver) root() (string, error) {
	raw := strings.TrimSpace(d.rootPath)
	if raw == "" {
		return "", errors.New("localstorage: empty path")
	}
	raw = os.ExpandEnv(raw)
	if strings.HasPrefix(raw, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			switch {
			case raw == "~":
				raw = home
			case strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`):
				raw = filepath.Join(home, raw[2:])
			}
		}
	}
	return filepath.Abs(raw)
}

func (d *Driver) pathForID(id string) (string, string, error) {
	root, err := d.root()
	if err != nil {
		return "", "", err
	}
	rel, err := decodeRel(id)
	if err != nil {
		return "", "", err
	}
	if rel == "" {
		return root, "", nil
	}
	p, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", "", err
	}
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", "", errors.New("localstorage: path escapes root")
	}
	return p, rel, nil
}

func decodeRel(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == "/" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("localstorage: invalid file id: %w", err)
	}
	rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(string(raw))))
	if rel == "." {
		return "", nil
	}
	if strings.HasPrefix(rel, "../") || rel == ".." || strings.HasPrefix(rel, "/") {
		return "", errors.New("localstorage: invalid relative path")
	}
	return rel, nil
}

func encodeRel(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if rel == "." || rel == "" {
		return "/"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(rel))
}

func idForRel(rel string) string {
	if rel == "" {
		return "/"
	}
	return encodeRel(rel)
}

func joinRel(parent, name string) string {
	if parent == "" {
		return filepath.ToSlash(name)
	}
	return filepath.ToSlash(filepath.Join(filepath.FromSlash(parent), name))
}

func parentRel(rel string) string {
	if rel == "" {
		return ""
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if parent == "." {
		return ""
	}
	return parent
}

func sizeForEntry(info os.FileInfo) int64 {
	if info == nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

var _ drives.Drive = (*Driver)(nil)
