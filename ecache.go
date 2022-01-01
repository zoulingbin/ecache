package ecache

import (
	"sync"
	"sync/atomic"
	"time"
)

var clock, p, n, bufferLen = time.Now().UnixNano(), uint16(0), uint16(1), int32(1000)

func now() int64 { return atomic.LoadInt64(&clock) }
func init() {
	go func() { // internal counter that reduce GC caused by `time.Now()`
		for {
			atomic.StoreInt64(&clock, time.Now().UnixNano()) // calibration every second
			for i := 0; i < 9; i++ {
				time.Sleep(100 * time.Millisecond)
				atomic.AddInt64(&clock, int64(100*time.Millisecond))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

type value struct {
	v *interface{}
	i int64
}

type node struct {
	k  string
	v  *value
	ts int64 // nano timestamp
}

type cache struct {
	dlnk [][2]uint16       // double link list, 0 for prev, 1 for next, the first node stands for [tail, head]
	m    []node            // memory pre-allocated
	last uint16            // last element index when not full
	hmap map[string]uint16 // key -> idx in []node
}

func create(cap int) *cache {
	return &cache{make([][2]uint16, cap+1), make([]node, cap), 0, make(map[string]uint16, cap)}
}

// put a cache item into lru cache, if added return 1, updated return 0
func (c *cache) put(k string, v *value, on inspector, free func(v **value) **value) int {
	if x, ok := c.hmap[k]; ok {
		*(free(&c.m[x-1].v)), c.m[x-1].ts = v, now()
		c.ajust(x, p, n) // refresh to head
		return 0
	}

	if c.last == uint16(cap(c.m)) {
		tail := &c.m[c.dlnk[0][p]-1]
		if (*tail).ts > 0 { // do not notify for mark delete ones
			on(PUT, (*tail).k, (*tail).v.v, (*tail).v.i, -1)
		}
		delete(c.hmap, (*tail).k)
		c.hmap[k], (*tail).k, *(free(&(*tail).v)), (*tail).ts = c.dlnk[0][p], k, v, now() // reuse to reduce gc
		c.ajust(c.dlnk[0][p], p, n)                                                       // refresh to head
		return 1
	}

	c.last++
	if len(c.hmap) <= 0 {
		c.dlnk[0][p] = c.last
	} else {
		c.dlnk[c.dlnk[0][n]][p] = c.last
	}
	c.m[c.last-1].k, c.m[c.last-1].v, c.m[c.last-1].ts, c.dlnk[c.last], c.hmap[k], c.dlnk[0][n] = k, v, now(), [2]uint16{0, c.dlnk[0][n]}, c.last, c.last
	return 1
}

// get value of key from lru cache with result
func (c *cache) get(k string) (*node, int) {
	if x, ok := c.hmap[k]; ok {
		c.ajust(x, p, n) // refresh to head
		return &c.m[x-1], 1
	}
	return nil, 0
}

// delete item by key from lru cache
func (c *cache) del(k string) (*node, int) {
	if x, ok := c.hmap[k]; ok {
		if c.m[x-1].ts > 0 {
			c.m[x-1].ts = 0  // mark as deleted
			c.ajust(x, n, p) // sink to tail
			return &c.m[x-1], 1
		}
	}
	return nil, 0
}

// calls f sequentially for each valid item in the lru cache
func (c *cache) walk(walker func(k string, v *interface{}, i int64, ts int64) bool) {
	for idx := c.dlnk[0][n]; idx != 0; idx = c.dlnk[idx][n] {
		if c.m[idx-1].ts > 0 && !walker(c.m[idx-1].k, c.m[idx-1].v.v, c.m[idx-1].v.i, c.m[idx-1].ts) {
			return
		}
	}
}

// when f=0, t=1, move to head, otherwise to tail
func (c *cache) ajust(idx, f, t uint16) {
	if c.dlnk[idx][f] != 0 { // f=0, t=1, not head node, otherwise not tail
		c.dlnk[c.dlnk[idx][t]][f], c.dlnk[c.dlnk[idx][f]][t], c.dlnk[idx][f], c.dlnk[idx][t], c.dlnk[c.dlnk[0][t]][f], c.dlnk[0][t] = c.dlnk[idx][f], c.dlnk[idx][t], 0, c.dlnk[0][t], idx, idx
	}
}

// hashCode hashes a string to a unique hashcode. BKDR hash as default
func hashCode(s string) (hash int32) {
	for i := 0; i < len(s); i++ {
		hash = hash*131 + int32(s[i])
	}
	return hash
}

func (c *Cache) alloc(v *interface{}, i int64) (val *value) {
	of := atomic.LoadInt32(&c.f)
	if nf := (of + 1) % bufferLen; of != atomic.LoadInt32(&c.r) && atomic.CompareAndSwapInt32(&c.f, of, nf) {
		val, c.ringbuf[nf] = c.ringbuf[nf], val
	}
	if val == nil { // even if get success from ringbuf, it also can be nil
		return &value{v: v, i: i}
	}
	val.v, val.i = v, i
	return val
}

func (c *Cache) free(v **value) **value {
	or := atomic.LoadInt32(&c.r)
	if nr := (or + 1) % bufferLen; atomic.CompareAndSwapInt32(&c.r, or, nr) {
		(*v).v, (*v).i, c.ringbuf[nr] = nil, 0, *v
	}
	return v
}

func (c *Cache) get(key string, idx, level int32) (*node, int) {
	if n, s := c.insts[idx][level].get(key); s > 0 && !((c.expiration > 0 && now()-n.ts > int64(c.expiration)) || n.ts <= 0) {
		return n, s // not necessary to remove the expired item here, otherwise will cause GC thrashing
	}
	return nil, 0
}

func nextPowOf2(cap int) int {
	if cap > 0 && cap&(cap-1) == 0 {
		return cap
	}
	return (cap | (cap >> 1) | (cap >> 2) | (cap >> 4) | (cap >> 8) | (cap >> 16)) + 1
}

// Cache - concurrent cache structure
type Cache struct {
	locks      []sync.Mutex
	insts      [][2]*cache // level-0 for normal LRU, level-1 for LRU-2
	ringbuf    []*value
	f, r, mask int32 // front & rear of ringbuf
	expiration time.Duration
	on         inspector
}

// NewLRUCache - create lru cache
// `bucketCnt` is buckets that shard items to reduce lock racing
// `capPerBkt` is length of each bucket, can store `capPerBkt * bucketCnt` count of items in Cache at most
// optional `expiration` is item alive time (and we only use lazy eviction here), default `0` stands for permanent
func NewLRUCache(bucketCnt int, capPerBkt int, expiration ...time.Duration) *Cache {
	size := nextPowOf2(bucketCnt)
	c := &Cache{make([]sync.Mutex, size), make([][2]*cache, size), make([]*value, bufferLen), int32(0), int32(0), int32(size - 1), 0, func(int, string, *interface{}, int64, int) {}}
	for i := range c.insts {
		c.insts[i][0] = create(capPerBkt)
	}
	if len(expiration) > 0 {
		c.expiration = expiration[0]
	}
	return c
}

// LRU2 - add LRU-2 support (especially LRU-2 that when item visited twice it moves to upper-level-cache)
// `capPerBkt` is length of each LRU-2 bucket, can store extra `capPerBkt * bucketCnt` count of items in Cache at most
func (c *Cache) LRU2(capPerBkt int) *Cache {
	for i := range c.insts {
		c.insts[i][1] = create(capPerBkt)
	}
	return c
}

// v - an interface value wrapper function for `PutV`
func (c *Cache) V(v interface{}) *value { return c.alloc(&v, 0) }

// i - an integer value wrapper function for `PutV`
func (c *Cache) I(i int64) *value { return c.alloc(nil, i) }

// PutV - put a item into cache
func (c *Cache) PutV(key string, val *value) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	status := c.insts[idx][0].put(key, val, c.on, c.free)
	c.locks[idx].Unlock()
	c.on(PUT, key, val.v, val.i, status)
}

// Put - put a item into cache
func (c *Cache) Put(key string, val interface{}) { c.PutV(key, c.V(val)) }

// Get - get value of key from cache with result
func (c *Cache) Get(key string) (interface{}, bool) {
	if v, i, b := c.GetV(key); b {
		if v != nil {
			return *v, b
		}
		return i, b // v.v is `nil` only when `int64` is valid
	}
	return nil, false
}

// GetV - get value of key from cache with result
func (c *Cache) GetV(key string) (*interface{}, int64, bool) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	n, s := (*node)(nil), 0
	if c.insts[idx][1] == nil { // (if LRU-2 mode not support, loss is little)
		n, s = c.get(key, idx, 0) // normal lru mode
	} else {
		n, s = c.insts[idx][0].del(key) // LRU-2 mode
		if s <= 0 {
			n, s = c.get(key, idx, 1) // re-find in level-1
		} else {
			c.insts[idx][1].put(key, n.v, c.on, c.free) // find in level-0, move to level-1
		}
	}
	c.locks[idx].Unlock()
	if s <= 0 {
		c.on(GET, key, nil, 0, 0)
		return nil, 0, false
	}
	c.on(GET, key, n.v.v, n.v.i, 1)
	return n.v.v, n.v.i, true
}

// Del - delete item by key from cache
func (c *Cache) Del(key string) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	n, s := c.insts[idx][0].del(key)
	if c.insts[idx][1] != nil { // (if LRU-2 mode not support, loss is little)
		n2, s2 := c.insts[idx][1].del(key)
		if n2 != nil && (n == nil || n.ts < n2.ts) { // callback latest added one if both exists
			n, s = n2, s2
		}
	}
	if s > 0 {
		c.on(DEL, key, n.v.v, n.v.i, 1)
		n.v.v = nil // release first
	} else {
		c.on(DEL, key, nil, 0, 0)
	}
	c.locks[idx].Unlock()
}

// Walk - calls f sequentially for each valid item in the lru cache, return false to stop iteration for every bucket
func (c *Cache) Walk(walker func(k string, v *interface{}, i int64, ts int64) bool) {
	for i := range c.insts {
		c.locks[i].Lock()
		c.insts[i][0].walk(walker)
		if c.insts[i][1] != nil {
			c.insts[i][1].walk(walker)
		}
		c.locks[i].Unlock()
	}
}

const (
	PUT = iota + 1
	GET
	DEL
)

// inspector - can be used to statistics cache hit/miss rate or other scenario like ringbuf queue
//   `action`:PUT, `status`: evicted=-1, updated=0, added=1
//   `action`:GET, `status`: miss=0, hit=1
//   `action`:DEL, `status`: miss=0, hit=1
//   `value` only valid when `status` is not 0 or `action` is PUT
type inspector func(action int, key string, value *interface{}, i int64, status int)

// Inspect - to inspect the actions
func (c *Cache) Inspect(insptr inspector) {
	old := c.on
	c.on = func(action int, key string, value *interface{}, i int64, status int) {
		old(action, key, value, i, status) // call as the declared order, old first
		insptr(action, key, value, i, status)
	}
}
