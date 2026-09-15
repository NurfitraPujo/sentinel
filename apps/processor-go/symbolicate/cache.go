package symbolicate

import (
	"container/list"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// CacheEventKind is the `kind` label of sentinel_sourcemap_cache_events_total (contract §12).
// Values are obs.SourcemapCache*, never re-typed here.
type CacheEventKind string

const (
	CacheHit         CacheEventKind = CacheEventKind(obs.SourcemapCacheHit)
	CacheMiss        CacheEventKind = CacheEventKind(obs.SourcemapCacheMiss)
	CacheNegativeHit CacheEventKind = CacheEventKind(obs.SourcemapCacheNegativeHit)
	CacheEvict       CacheEventKind = CacheEventKind(obs.SourcemapCacheEvict)
	// CacheOversize is a map larger than the CACHE bound: fetched, parsed, handed to the event,
	// not retained. A row over the PARSE cap is never fetched and is a frame outcome (`oversize`),
	// not this.
	CacheOversize CacheEventKind = CacheEventKind(obs.SourcemapCacheOversize)
)

// negKey is the negative cache's key (§4.4, cache paragraph): the lookup triple, so a miss on
// the database and a failure after it are both remembered against what the next event asks for.
type negKey struct {
	projectID, releaseVersion, minifiedPath string
}

type posEntry struct {
	row     *Sourcemap
	key     negKey
	pm      *parsedMap
	cost    int64
	expires time.Time
	elem    *list.Element
}

type negEntry struct {
	reason  Outcome
	expires time.Time
}

// cache is the per-pod, byte-bounded LRU of §4.4's cache paragraph plus the negative entries.
//
//   - positive entries are keyed by sourcemaps.id, cost = the row's raw size_bytes (never the
//     parsed footprint -- the gauge accounts raw bytes, stated), evicted least-recently-used
//     past cfg.CacheBytes and lazily on read past cfg.PositiveTTL. Each is also indexed by the
//     lookup triple it was found under, so the hot path issues the database query ONCE per map
//     on a hit (§4.4 step 3; S9: one SELECT + one GET per map, then hits only) -- a replaced map
//     (same triple, new bytes) is therefore seen when the entry is evicted or expires, which is
//     exactly the propagation bound the contract states (Known limit 3);
//   - negative entries are keyed by the lookup triple and carry the reason they were cached with
//     (`no_map`, `fetch_error`, `map_error`, `oversize` -- never `timeout`), re-emitted on every
//     hit until cfg.NegativeTTL passes; they are small and bounded by the traffic of one TTL, and
//     expired ones are swept whenever one is added.
//
// now is injectable so the TTL tests do not sleep.
type cache struct {
	mu       sync.Mutex
	bound    int64
	posTTL   time.Duration
	negTTL   time.Duration
	now      func() time.Time
	observer Observer

	bytes      int64
	lru        *list.List           // front = most recently used
	pos        map[string]*posEntry // by sourcemaps.id
	byKey      map[negKey]*posEntry // the same entries by lookup triple
	neg        map[negKey]negEntry
	negSweepAt int
}

// negSweepMin is the smallest negative-map size at which setNegative sweeps expired entries.
const negSweepMin = 1024

func newCache(cfg Config, now func() time.Time, obs Observer) *cache {
	return &cache{
		bound:    cfg.CacheBytes,
		posTTL:   cfg.PositiveTTL,
		negTTL:   cfg.NegativeTTL,
		now:      now,
		observer: obs,
		lru:      list.New(),
		pos:      map[string]*posEntry{},
		byKey:    map[negKey]*posEntry{},
		neg:      map[negKey]negEntry{},

		negSweepAt: negSweepMin,
	}
}

// get returns the row and parsed map cached under a lookup triple, counting a hit or a miss. An
// entry past its positive TTL is removed and counted as an eviction and a miss.
func (c *cache) get(k negKey) (*Sourcemap, *parsedMap, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[k]
	if !ok {
		c.observer.CacheEvent(CacheMiss)
		return nil, nil, false
	}
	if !c.now().Before(e.expires) {
		c.removeLocked(e)
		c.observer.CacheEvent(CacheEvict)
		c.observer.CacheEvent(CacheMiss)
		return nil, nil, false
	}
	c.lru.MoveToFront(e.elem)
	c.observer.CacheEvent(CacheHit)
	return e.row, e.pm, true
}

// put retains a parsed map under the byte bound, evicting least-recently-used entries to make
// room. cost is the row's size_bytes. A map whose cost alone exceeds the bound is not retained
// (CacheOversize) and the caller uses the parsed map for this event only.
func (c *cache) put(k negKey, row *Sourcemap, pm *parsedMap) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cost := row.SizeBytes
	if cost > c.bound {
		c.observer.CacheEvent(CacheOversize)
		return
	}
	if old, ok := c.pos[row.ID]; ok {
		c.removeLocked(old)
	}
	if old, ok := c.byKey[k]; ok {
		c.removeLocked(old)
	}
	for c.bytes+cost > c.bound && c.lru.Len() > 0 {
		c.removeLocked(c.lru.Back().Value.(*posEntry))
		c.observer.CacheEvent(CacheEvict)
	}
	e := &posEntry{row: row, key: k, pm: pm, cost: cost, expires: c.now().Add(c.posTTL)}
	e.elem = c.lru.PushFront(e)
	c.pos[row.ID] = e
	c.byKey[k] = e
	c.bytes += cost
	c.observer.CacheBytes(c.bytes)
}

// drop removes a positive entry (the map's resolve panicked after it was cached, decision 19).
func (c *cache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.pos[id]; ok {
		c.removeLocked(e)
		c.observer.CacheEvent(CacheEvict)
	}
}

func (c *cache) removeLocked(e *posEntry) {
	c.lru.Remove(e.elem)
	delete(c.pos, e.row.ID)
	delete(c.byKey, e.key)
	c.bytes -= e.cost
	c.observer.CacheBytes(c.bytes)
}

// negative returns the reason a triple was negatively cached with, counting a negative hit; an
// expired entry is dropped and reads as absent.
func (c *cache) negative(k negKey) (Outcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.neg[k]
	if !ok {
		return "", false
	}
	if !c.now().Before(e.expires) {
		delete(c.neg, k)
		return "", false
	}
	c.observer.CacheEvent(CacheNegativeHit)
	return e.reason, true
}

// setNegative records a reason for a triple for the negative TTL. reason must be one of the four
// cacheable outcomes; `timeout` is refused (decision 37: the fill is in flight, and a negative
// entry would keep the map unreachable after it landed) -- the caller's guard, re-checked here.
func (c *cache) setNegative(k negKey, reason Outcome) {
	if !reason.negativelyCacheable() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	// Amortised sweep: when the map reaches the high-water mark, drop every expired entry and set
	// the next mark at twice what survived (floor negSweepMin), so a burst of distinct triples costs
	// O(n) once per doubling rather than per add, and the map stays bounded by roughly two TTLs of
	// distinct traffic.
	if len(c.neg) >= c.negSweepAt {
		for key, e := range c.neg {
			if !now.Before(e.expires) {
				delete(c.neg, key)
			}
		}
		c.negSweepAt = max(negSweepMin, 2*len(c.neg))
	}
	c.neg[k] = negEntry{reason: reason, expires: now.Add(c.negTTL)}
}

// Bytes is the current positive-cache cost (the gauge of §12).
func (c *cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
