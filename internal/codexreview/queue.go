// Package codexreview runs a private, local Codex review worker for danmaku.
package codexreview

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"comment-bridge/internal/commentbridge"
	"comment-bridge/internal/runlock"
	"comment-bridge/internal/secretfile"
)

const maxQueueBytes = 2 << 20

type Queue struct{ Dir string }

func NewQueue(dir string) (*Queue, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("danmaku queue needs an absolute directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("danmaku queue must be an existing private 0700 directory")
	}
	return &Queue{Dir: dir}, nil
}

func (q *Queue) path(id, suffix string) (string, error) {
	if raw, err := hex.DecodeString(id); err != nil || len(raw) != 32 || strings.ToLower(id) != id {
		return "", errors.New("invalid danmaku batch ID")
	}
	return filepath.Join(q.Dir, id+suffix), nil
}

func (q *Queue) Submit(id string, items []commentbridge.Danmaku) error {
	path, err := q.path(id, ".request.json")
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeJSON(path, items)
}

func (q *Queue) Result(id string) (review commentbridge.DanmakuReview, ready bool, err error) {
	path, err := q.path(id, ".result.json")
	if err != nil {
		return review, false, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return review, false, nil
	} else if err != nil {
		return review, false, err
	}
	err = readJSON(path, &review)
	return review, err == nil, err
}

func (q *Queue) Remove(id string) error {
	for _, suffix := range []string{".request.json", ".claim", ".result.json"} {
		path, err := q.path(id, suffix)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// WorkOnce holds a host-only lock for the whole inference. An interrupted claim
// becomes a failed result on the next invocation, never an automatic replay.
func (q *Queue) WorkOnce(ctx context.Context, review func(context.Context, []commentbridge.Danmaku) (commentbridge.DanmakuReview, error)) error {
	lock, err := runlock.Acquire(filepath.Join(q.Dir, "worker"))
	if errors.Is(err, runlock.ErrLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	entries, err := os.ReadDir(q.Dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".request.json")
		if !ok {
			continue
		}
		requestPath, err := q.path(id, ".request.json")
		if err != nil {
			return err
		}
		if _, ready, err := q.Result(id); err != nil || ready {
			if err != nil {
				return err
			}
			continue
		}
		claimPath, _ := q.path(id, ".claim")
		resultPath, _ := q.path(id, ".result.json")
		claim, err := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			return writeJSON(resultPath, commentbridge.DanmakuReview{Failed: true})
		}
		if err != nil {
			return err
		}
		if err := claim.Close(); err != nil {
			return err
		}
		var items []commentbridge.Danmaku
		var result commentbridge.DanmakuReview
		err = readJSON(requestPath, &items)
		if err == nil {
			err = validateItems(items)
		}
		if err == nil {
			result, err = review(ctx, items)
		}
		if err == nil {
			err = commentbridge.ValidateDanmakuReview(items, result)
		}
		if err != nil {
			result = commentbridge.DanmakuReview{Failed: true}
		}
		return writeJSON(resultPath, result)
	}
	return nil
}

func validateItems(items []commentbridge.Danmaku) error {
	if len(items) == 0 || len(items) > commentbridge.DanmakuLimit {
		return errors.New("invalid danmaku batch size")
	}
	ids := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || len(item.ID) > 32 || ids[item.ID] || item.Text == "" || len([]rune(item.Text)) > 2000 || len([]rune(item.VideoTitle)) > 500 {
			return errors.New("invalid danmaku batch item")
		}
		ids[item.ID] = true
	}
	return nil
}

func readJSON(path string, target any) error {
	data, err := secretfile.Load(path, maxQueueBytes)
	if err != nil {
		return errors.New("cannot read private danmaku queue file")
	}
	return decodeJSON(data, target)
}

func decodeJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return errors.New("invalid danmaku JSON")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing danmaku JSON")
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxQueueBytes {
		return errors.New("cannot encode danmaku queue file")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".danmaku-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
