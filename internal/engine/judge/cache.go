package judge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const DefaultCachePath = ".axda/judge-cache.json"

// Cache stores judge verdicts keyed by model, effort, and prompt hash.
//
// A judge is non-deterministic, so re-running the same evaluation would
// otherwise produce a different report each time and cost money each time.
// Caching makes repeated runs over an unchanged trace stable in practice —
// it does not make judges deterministic, which is why they stay advisory.
type Cache struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Verdict
	dirty   bool
}

func OpenCache(path string) *Cache {
	if path == "" {
		path = DefaultCachePath
	}
	c := &Cache{path: path, entries: map[string]*Verdict{}}

	b, err := os.ReadFile(path)
	if err != nil {
		return c // absent or unreadable cache is simply a cold cache
	}
	var entries map[string]*Verdict
	if err := json.Unmarshal(b, &entries); err == nil && entries != nil {
		c.entries = entries
	}
	return c
}

func (c *Cache) Get(key string) (*Verdict, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	copied := *v
	return &copied, true
}

func (c *Cache) Put(key string, v *Verdict) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := *v
	copied.Cached = false
	c.entries[key] = &copied
	c.dirty = true
}

// Flush persists the cache. A write failure is not an evaluation failure —
// the report is already correct — so the error is returned for logging only.
func (c *Cache) Flush() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path, b, 0o600); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

func (j *Judge) Flush() error {
	if j == nil || j.cache == nil {
		return nil
	}
	return j.cache.Flush()
}
