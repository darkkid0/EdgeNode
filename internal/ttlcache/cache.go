package ttlcache

import (
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/TeaOSLab/EdgeNode/internal/utils/fasttime"
)

var SharedCache = NewBigCache()

// Cache TTL缓存
// 最大的缓存时间为30 * 86400
// Piece数据结构：
//
//	    Piece1            |  Piece2 | Piece3 | ...
//	[ Item1, Item2, ... ] |   ...
//
type Cache struct {
	isDestroyed bool
	pieces      []*Piece
	countPieces uint64
	maxItems    int

	gcPieceIndex int
}

func NewBigCache() *Cache {
	var delta = utils.SystemMemoryGB() / 2
	if delta <= 0 {
		delta = 1
	}
	return NewCache(NewMaxItemsOption(delta * 1_000_000))
}

func NewCache(opt ...OptionInterface) *Cache {
	var countPieces = 256
	var maxItems = 1_000_000

	var totalMemory = utils.SystemMemoryGB()
	if totalMemory < 2 {
		// 我们限制内存过小的服务能够使用的数量
		maxItems = 500_000
	} else {
		var delta = totalMemory / 4
		if delta > 0 {
			maxItems *= delta
		}
	}

	for _, option := range opt {
		if option == nil {
			continue
		}
		switch o := option.(type) {
		case *PiecesOption:
			if o.Count > 0 {
				countPieces = o.Count
			}
		case *MaxItemsOption:
			if o.Count > 0 {
				maxItems = o.Count
			}
		}
	}

	var cache = &Cache{
		countPieces: uint64(countPieces),
		maxItems:    maxItems,
	}

	for i := 0; i < countPieces; i++ {
		cache.pieces = append(cache.pieces, NewPiece(maxItems/countPieces))
	}

	// Add to manager
	SharedManager.Add(cache)

	return cache
}

func (this *Cache) Write(key string, value any, expiredAt int64) (ok bool) {
	if this.isDestroyed {
		return
	}

	var currentTimestamp = fasttime.Now().Unix()
	if expiredAt <= currentTimestamp {
		return
	}

	var maxExpiredAt = currentTimestamp + 30*86400
	if expiredAt > maxExpiredAt {
		expiredAt = maxExpiredAt
	}
	var uint64Key = HashKey([]byte(key))
	var pieceIndex = uint64Key % this.countPieces
	return this.pieces[pieceIndex].Add(uint64Key, &Item{
		Value:     value,
		expiredAt: expiredAt,
	})
}

func (this *Cache) IncreaseInt64(key string, delta int64, expiredAt int64, extend bool) int64 {
	if this.isDestroyed {
		return 0
	}

	var currentTimestamp = fasttime.Now().Unix()
	if expiredAt <= currentTimestamp {
		return 0
	}

	var maxExpiredAt = currentTimestamp + 30*86400
	if expiredAt > maxExpiredAt {
		expiredAt = maxExpiredAt
	}
	var uint64Key = HashKey([]byte(key))
	var pieceIndex = uint64Key % this.countPieces
	return this.pieces[pieceIndex].IncreaseInt64(uint64Key, delta, expiredAt, extend)
}

func (this *Cache) Read(key string) (item *Item) {
	var uint64Key = HashKey([]byte(key))
	return this.pieces[uint64Key%this.countPieces].Read(uint64Key)
}

func (this *Cache) readIntKey(key uint64) (value *Item) {
	return this.pieces[key%this.countPieces].Read(key)
}

func (this *Cache) Delete(key string) {
	var uint64Key = HashKey([]byte(key))
	this.pieces[uint64Key%this.countPieces].Delete(uint64Key)
}

func (this *Cache) deleteIntKey(key uint64) {
	this.pieces[key%this.countPieces].Delete(key)
}

func (this *Cache) Count() (count int) {
	for _, piece := range this.pieces {
		count += piece.Count()
	}
	return
}

func (this *Cache) GC() {
	var index = this.gcPieceIndex
	const maxPiecesPerGC = 4
	for i := index; i < index+maxPiecesPerGC; i++ {
		if i >= int(this.countPieces) {
			break
		}
		this.pieces[i].GC()
	}

	index += maxPiecesPerGC
	if index >= int(this.countPieces) {
		index = 0
	}
	this.gcPieceIndex = index
}

func (this *Cache) Clean() {
	for _, piece := range this.pieces {
		piece.Clean()
	}
}

func (this *Cache) Destroy() {
	SharedManager.Remove(this)

	this.isDestroyed = true

	for _, piece := range this.pieces {
		piece.Destroy()
	}
}
