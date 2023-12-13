package remote

import (
	"fmt"
	"os"
	"path/filepath"
)

const defaultPath = "/tmp/lightning/remote/chunks/%s/%d/"

type chunkCache struct {
	chunks  map[uint64]int // chunkID -> chunkSize
	baseDir string
}

func newChunkCache(loadDataTaskID string, writerID uint64) (*chunkCache, error) {
	baseDir := fmt.Sprintf(defaultPath, loadDataTaskID, writerID)
	err := os.MkdirAll(baseDir, 0o755)
	if err != nil {
		return nil, err
	}
	return &chunkCache{
		chunks:  map[uint64]int{},
		baseDir: baseDir,
	}, nil
}

func (c *chunkCache) get(chunkID uint64) ([]byte, error) {
	path := filepath.Join(c.baseDir, fmt.Sprintf("chunk-%d", chunkID))
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, c.chunks[chunkID])
	n, err := file.Read(buf)
	if err != nil {
		return nil, err
	}
	buf = buf[:n]

	return buf, file.Close()
}

func (c *chunkCache) put(chunkID uint64, buf []byte) error {
	fileName := filepath.Join(c.baseDir, fmt.Sprintf("chunk-%d", chunkID))
	file, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	_, err = file.Write(buf)
	if err != nil {
		return err
	}

	c.chunks[chunkID] = len(buf)
	return file.Close()
}

func (c *chunkCache) clean(chunkID uint64) error {
	if _, ok := c.chunks[chunkID]; !ok {
		return nil
	}
	delete(c.chunks, chunkID)
	fileName := filepath.Join(c.baseDir, fmt.Sprintf("chunk-%d", chunkID))
	return os.Remove(fileName)
}

func (c *chunkCache) cleanAll() error {
	return os.RemoveAll(c.baseDir)
}
